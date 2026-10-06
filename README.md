# calmerge

A tiny Go sidecar that fetches multiple ICS/iCal feeds, merges them into one
chronological, source-tagged list, and serves it as JSON for a
[Glance](https://github.com/glanceapp/glance) `custom-api` widget.

Glance can't read ICS on its own (the built-in `calendar` widget is just a
month grid, and `custom-api` only speaks JSON), so this bridges the gap and
also does the thing the existing community ICS widgets don't: **merge several
feeds into a single list with a per-calendar tag.**

## What it does

- Pulls each feed concurrently, expands recurring events (RRULE) and converts
  to your local timezone via [gocal](https://github.com/apognu/gocal). Microsoft
  Windows zone names (e.g. "Eastern Standard Time") that Outlook/O365 feeds emit
  are remapped to IANA names first, otherwise the parser falls back to UTC and
  times come out hours off.
- Floating all-day events (`VALUE=DATE`) are pinned to local midnight so they
  land on the right day instead of shifting to the night before.
- Groups events by day with `Today` / `Tomorrow` / weekday labels.
- Tags each event with its source calendar name + color.
- Refreshes feeds on a background timer (default every 15m) and always serves
  the last-known payload instantly, so requests never block on the upstream
  fetch (this is what keeps Glance from timing out while "awaiting headers").
- If one feed fails, the others still render and the failure shows up in
  `errors`. A failed fetch is retried once; if that fails too, the feed's last
  good events stay in the payload (for up to a day) and `errors` says how old
  they are, so a blip on one calendar host never empties that calendar.

## Endpoints

- `GET /events` - the merged JSON (see shape below)
  - `?days=N` - trim the payload to the next `N` calendar days (today inclusive),
    e.g. `?days=2` for a today+tomorrow briefing. Past/lookback days are dropped
    when this is set. Invalid/omitted → the full window.
- `GET /ui` - the review page (see [Review UI](#review-ui)); `/` redirects there
- `POST /mcp` - MCP server (Streamable HTTP; read-only apart from entity corrections), see
  [MCP endpoint](#mcp-endpoint)
- `GET /healthz` - returns `ok` (never authenticated; used for container/tunnel health)

### Authentication

`/events` and `/mcp` are **token-free for internal requests** (the Glance widget over the
Docker network) and **bearer-token protected for tunnel traffic**. Cloudflare
stamps every tunnel-proxied request with a `Cf-Connecting-Ip` header, which is
what triggers enforcement — so internal callers need nothing, while anything
arriving through the tunnel must send:

```
Authorization: Bearer <AUTH_TOKEN>
```

This fails closed: if `AUTH_TOKEN` is unset, tunnel requests are rejected with
`401` rather than served openly. See
[Exposing to Claude via Cloudflare Tunnel](#exposing-to-claude-via-cloudflare-tunnel).

**Keep the port private, or turn on `require_auth`.** By default the token
check keys off Cloudflare's headers, so anything else that can reach the port
reads `/events` and `/mcp` without a token: a published port, a different
reverse proxy (nginx, Caddy, Traefik), or anyone on the same network. Either
bind it to loopback or an internal Docker network (the bundled `compose.yml`
does) and only expose it through a Cloudflare tunnel, or set
`require_auth = true` (`REQUIRE_AUTH=true`) so every request needs the token,
Glance included (add the `Authorization` header to the widget). Feed URLs are
kept out of responses and errors, but event details (titles, attendees,
agendas) are served.

Response shape:

```json
{
  "generated": "2026-05-29T19:14:52-04:00",
  "count": 44,
  "days": [
    { "label": "Today · Fri, May 29", "date": "2026-05-29", "events": [
      { "name": "...", "start": "2026-05-29T05:30:00-04:00", "end": "...",
        "date": "2026-05-29", "allDay": false, "ongoing": false,
        "location": "Microsoft Teams Meeting", "source": "Work", "color": "#5aa2f0",
        "seriesId": "a1b2c3d4e5f6",
        "organizer": "Jane Doe",
        "attendees": [ { "name": "Jane Doe", "email": "jane@example.com", "status": "accepted", "role": "organizer" } ],
        "agenda": "1. Roadmap\n2. Q&A" }
    ]}
  ],
  "events": [ ... flat list, same objects ... ],
  "errors": []
}
```

Optional per-event fields (all `omitempty`, so absent when empty/disabled):

| Field | When | Notes |
|---|---|---|
| `seriesId` | always | Stable id (12-hex of the ICS `UID`) shared by every instance of one source event — group recurring/multi-day instances by it. |
| `organizer` | `INCLUDE_ATTENDEES` | Organizer display name (or email). Falls back to the DESCRIPTION roster when the feed has no `ORGANIZER` property. |
| `attendees` | `INCLUDE_ATTENDEES` | `[{name,email,status,role}]`; status is `accepted`/`declined`/`tentative`/`needs-action`, role is `organizer`/`required`/`optional`. Merged from structured `ATTENDEE` properties **and** the Google "Guests" roster embedded in the DESCRIPTION (published Outlook feeds strip `ATTENDEE` but keep that text) — structured data wins, the roster fills gaps. |
| `agenda` | `INCLUDE_AGENDA` | `DESCRIPTION` text with Teams/Zoom/Meet join boilerplate stripped (join URL is already in `meetUrl`). |
| `startDate`/`endDate` | all-day events | True **inclusive** span as `YYYY-MM-DD`, never clipped to the fetch window; identical on every per-day instance. |
| `multiDay`/`spanDays` | all-day events | `spanDays > 1` and the inclusive day count of the span. |
| `dayIndex` | all-day events | 1-based position of this instance within the true span (day 2 of 5). |
| `status` | feed has it | `STATUS` lowercased: `confirmed` or `tentative`. Cancelled meetings are left out entirely. |
| `showAs` | Outlook feeds | `X-MICROSOFT-CDO-BUSYSTATUS` lowercased: `free`/`tentative`/`busy`/`oof`/`workingelsewhere`. |
| `transparent` | `TRANSP:TRANSPARENT` | `true` when the event doesn't block time (marked free, or declined and kept). |
| `myResponse` | `self_emails` set | Your own RSVP (`accepted`/`declined`/`tentative`/`needs-action`), from the `ATTENDEE` matching one of your addresses. Only feeds that keep `ATTENDEE` (Google, iCloud) have it; published Outlook feeds don't. |
| `categories` | feed has them | Raw ICS `CATEGORIES` (Outlook's manual categories). |
| `entity` / `entityColor` / `entityVia` | entities configured | Business/client the event is for, see [Entity tagging](#entity-tagging). |

With entities configured, the response also carries a top-level `entities` list
(`{name,parent,color,url}`) so each event only needs the name.

## Configuration

Settings live in a TOML file, `/config/calmerge.toml` by default (`CONFIG_FILE`
to move it). Start from [`calmerge.example.toml`](calmerge.example.toml),
which has every key, comments, and a sample entity list.

- **Hot reload.** The file is checked every 5 seconds; edits apply without a
  restart and kick an immediate refresh. A broken edit is logged and ignored,
  and the last good config keeps serving.
- **Env vars still work.** Anything the file leaves out falls back to its env
  var, then the default, so the file can take over one setting at a time. With
  no file at all, calmerge runs on env vars exactly as before.
- **Keep it off `/data`.** That's the volume for corrections and lessons; a
  file mounted inside it would be hidden by the volume mount.

| File key | Env var | Default | Notes |
|---|---|---|---|
| `[[feeds]]` | `FEEDS` | (required) | `{name,url,color?,user?,pass?}`; env takes a JSON array. |
| `tz` | `TZ_NAME` | `America/New_York` | IANA tz used for display + day grouping. |
| `lookahead_days` | `LOOKAHEAD_DAYS` | `30` | How far forward to include. |
| `lookback_days` | `LOOKBACK_DAYS` | `0` | Bump to 1-2 to catch multi-day/ongoing events that started earlier. |
| `refresh_minutes` | `CACHE_TTL_MIN` | `15` | Background refresh interval in minutes. |
| `http_timeout_seconds` | `HTTP_TIMEOUT_SEC` | `20` | Per-feed fetch timeout. |
| `include_attendees` | `INCLUDE_ATTENDEES` | `true` | Include the per-event `attendees` list and `organizer`. Set `false` to trim the payload. |
| `include_agenda` | `INCLUDE_AGENDA` | `true` | Include the per-event cleaned `agenda` (DESCRIPTION minus conferencing boilerplate). Set `false` to trim the payload. |
| `require_auth` | `REQUIRE_AUTH` | `false` | Require the bearer token on every `/events` and `/mcp` request, not just tunnel traffic. Turn it on whenever something other than a Cloudflare tunnel can reach the port. |
| `self_emails` | `SELF_EMAILS` | (none) | Your own addresses, so each event's `myResponse` can carry your RSVP. Env takes a comma-separated list. |
| `skip_declined` | `SKIP_DECLINED` | `false` | Leave out meetings whose `myResponse` is `declined`. Needs `self_emails`. |
| `[[entities]]` | `ENTITIES` | (none) | Businesses/clients to tag events with; env takes a JSON array. None = tagging off. See [Entity tagging](#entity-tagging). |
| `corrections_file` | `CORRECTIONS_FILE` | `/data/corrections.json` | Where entity corrections are saved. |
| `lessons_file` | `LESSONS_FILE` | `/data/lessons.json` | The classifier's long-term memory. |
| `lessons_max_age_days` | `LESSONS_MAX_AGE_DAYS` | `365` | Forget lessons not seen this long. `0` = never. |
| (env only) | `LISTEN` | `:8076` | Listen address. Needs a restart anyway. |
| (env only) | `AUTH_TOKEN` | (empty) | Bearer token required for tunnel traffic. Empty = API stays internal-only (tunnel requests get `401`). Generate with `openssl rand -hex 32`. Kept out of the file because it's a secret. |
| (env only) | `CONFIG_FILE` | `/config/calmerge.toml` | Where to find the config file. |

Mount a volume at `/data` so corrections and lessons survive redeploys.

### Moving from env vars to the file

`calmerge -print-config` prints the settings it's running with (file plus env)
as a ready-to-mount `calmerge.toml`, leaving out `AUTH_TOKEN` and `LISTEN`.
`-env-file` loads env vars from a file first, read the way Docker reads an
env file, so JSON values like `FEEDS` survive as-is (no shell quoting).

1. Copy calmerge's env vars from your host (Dokploy, Portainer, a `.env`)
   into a local file, e.g. `~/calmerge.env`.
2. In the repo: `go run . -env-file ~/calmerge.env -print-config > calmerge.toml`
   (`calmerge.toml` is git-ignored; it holds your feed URLs).
3. Add the `[[entities]]` blocks from `calmerge.example.toml` if your env
   didn't have `ENTITIES`.
4. Mount it at `/config/calmerge.toml` (a file mount on your host), then
   delete the moved vars from the env, keeping `AUTH_TOKEN`. Delete
   `~/calmerge.env`.

Feeds without a `color` take a default palette in order (blue, green, pink,
amber, teal, coral, then around again); set `color` to pick your own.

## Entity tagging

Tags each event with the business or client it's for, so the calendar joins up
with wherever you plan the work (a wiki, a project tool). Name entities the way
those docs do; `parent` records a rollup (e.g. the holding company a client
belongs to), and `url` links the entity's page from the review UI. It's all
built in: plain Go, no model files, no outside service.

```toml
[[entities]]
name = "Northwind Holdings"
feeds = ["Work"]

[[entities]]
name = "Globex Records"
parent = "Northwind Holdings"
color = "#dc4747"
url = "https://wiki.example.com/globex"
keywords = ["Globex", "Initech"]
categories = ["Globex"]

[[entities]]
name = "Hooli"
parent = "Northwind Holdings"
keywords = ["Hooli"]
domains = ["hooli.example"]
```

Each refresh, every event goes through these steps, first hit wins.
`entityVia` says which one decided it:

0. **`manual`**: you corrected this series with the `set_event_entity` MCP
   tool. Beats everything, and teaches the model (step 3) about look-alikes.
1. **`category`**: the event has an Outlook category mapped to an entity
   (`categories`, or the entity name itself). Published Outlook feeds don't
   currently include categories, but other feeds may.
2. **`rule`**: keyword and email-domain scoring. A keyword in the title (+3) or
   the organizer's domain (+3) is enough alone; keywords in the agenda or
   location (+1) and other attendees' domains (+1) only nudge. The top score
   must be at least 2 and beat the runner-up, so ties stay open.
3. **`learned`**: a small naive Bayes model, rebuilt each refresh from every
   firm label (steps 0-2), both in the current window and remembered from the
   past (see [Long-term memory](#long-term-memory)). One sample per recurring
   series. It learns from title and agenda words, the individual attendees and
   their domains, the organizer, and the Teams tenant id in the join link. So
   a meeting with people you've met on Globex calls lands on Globex, and
   a meeting on a client's Teams tenant lands on that client with no keyword. It only
   answers at 85%+ confidence with at least two familiar features. Published
   Outlook feeds strip most attendee lists, so people only help where the feed
   has them.
4. **`feed`**: the feed's fallback entity (`feeds`), e.g. everything else on
   the Work calendar rolls up to Northwind Holdings.

Anything still unplaced has no `entity`. Treat `learned` and `feed` as guesses.

### Corrections

When a tag is wrong, fix it once and it sticks. The MCP tool
`set_event_entity` takes an event's `seriesId` and an entity name (or `none`
to remove the fix), saves it to `corrections_file`, and kicks an immediate
refresh. The fix covers every instance of a recurring series and trains the
model, so similar meetings (same people, same Teams tenant, similar titles)
start landing right too. In practice you tell Claude "the offsite is Globex" during
a briefing and it calls the tool.

The tool always requires the bearer token, even on the internal network
(nothing internal ever writes), and refuses while no entities are configured.
Pins to an entity later removed from the config are ignored. The file is plain
JSON keyed by `seriesId`, with the event title alongside for readability.

### Long-term memory

The fetch window only covers the next `lookahead_days`, but ongoing work spans
months. So every firm label (manual, category, rule) is saved to
`lessons_file` along with the clues the model learned from it. Those lessons
keep teaching after the meeting leaves the window, until they go unseen for
`lessons_max_age_days`.

What's on the calendar now always beats memory: if a series is back in the
window with no firm label (say its correction was removed), its lesson is
dropped. The file is only rewritten when something changed.

## Review UI

`/ui` is a small web app for checking and fixing entity tags without going
through Claude. Server-rendered Go templates, [htmx 4](https://four.htmx.org)
(with the `hx-live` extension) and Tailwind; everything is embedded in the
binary.

- **Agenda** (default) and **Week** views of the calendar, each meeting tagged
  `calendar › client`. Guesses (the model, or a calendar default) have a dashed
  amber pill and a one-click **Confirm**; the **Guesses** filter shows only
  those. Free time between meetings shows as a zig-zag divider; time off (OOO,
  PTO, vacation, holidays, and other people's "Name - Vacation Alert") gets
  its own hatched banner instead of a client tag.
- Picking a meeting opens its details: when, how it repeats, Join / calendar /
  docs links, why it was tagged, people grouped by company, the agenda,
  and a client picker to pin the whole series. Corrections apply instantly
  (the cached events are re-tagged in memory) and show a toast with Undo.
- The header pill shows freshness, turns into a loader while feeds refresh,
  and refreshes on click. When the data changes underneath (a refresh, or a
  correction made through MCP), the open page updates itself.
- **Sign in** with `AUTH_TOKEN`; the page sets an HttpOnly cookie derived from
  it (rotating the token signs everyone out). With no `AUTH_TOKEN` the UI is
  closed. Writes also require the `HX-Request` header, so a form on another
  site can't post them. Wrong tokens are limited to 5 a minute per client
  (30 a minute overall); past that, sign-in waits for the next minute.
- No outside requests: the fonts (IBM Plex, OFL) ship in the binary too.
- Motion: view transitions for paging and the details panel, a real CSS morph
  for unfolding today's finished meetings, a slide-up sheet on phones.
  Everything respects `prefers-reduced-motion`.

**Working on it:** `ui/tailwind.css` builds to the committed
`ui/static/app.css` with the standalone Tailwind v4 CLI
(`brew install tailwindcss`), via `go generate ./...`. Rebuild after touching
templates or the CSS source. To run it on real data without the feed URLs,
save an `/events` payload and start with `-seed`:

```bash
curl -s https://calmerge.example.com/events -H "Authorization: Bearer $AUTH_TOKEN" > /tmp/events.json
```

```bash
FEEDS='[{"name":"Work","url":"http://127.0.0.1:1/"}]' AUTH_TOKEN=dev LISTEN=127.0.0.1:18080 go run . -seed /tmp/events.json
```

## Deploy with Docker

Images for `linux/amd64` and `linux/arm64` are published to the GitHub
container registry: `ghcr.io/drudge/calmerge:latest` follows `main`, and
releases are tagged by version (`:1.2.3`, `:1.2`). The image is distroless and
runs as a non-root user.

```bash
docker run -d --name calmerge -p 127.0.0.1:8076:8076 \
  -v "$PWD/calmerge.toml:/config/calmerge.toml:ro" -v calmerge-data:/data \
  -e AUTH_TOKEN="$AUTH_TOKEN" ghcr.io/drudge/calmerge:latest
```

The steps below use [Dokploy](https://dokploy.com) words (File Mount, Volume),
but any Docker host works the same way. You have two options.

### A) Add calmerge to your existing Glance stack (recommended)

Both containers need to be on the same Docker network so Glance can reach
`http://calmerge:8076`. Easiest is to put calmerge in the same compose project
as Glance:

1. Drop the `calmerge` service from `compose.yml` into your Glance stack's
   compose (or just deploy this whole `compose.yml`, which includes Glance).
2. Add a **File Mount** at `/config/calmerge.toml` with your config (start
   from `calmerge.example.toml`), and a **Volume** at `/data` for corrections
   and lessons. Set `AUTH_TOKEN` as an env var. (Env-only still works: set
   `FEEDS` from `.env.example` instead of the file.)
3. Paste the widget block from `glance-widget.yml` into a column in your
   `glance.yml`.
4. Redeploy. Glance's `custom-api` widget will hit calmerge over the internal
   network.

### B) Run calmerge as its own app

Deploy it standalone, then attach it to the same Docker network Glance uses
(e.g. add both to an external network), and point the widget `url` at the
service name or its address on that shared network.

## Exposing to Claude via Cloudflare Tunnel

To let a Claude skill (e.g. a daily-briefing) replace its own ICS parsing with a
single `GET /events` call, expose calmerge through your existing Cloudflare
tunnel. The tunnel path is bearer-authenticated; the internal Glance path is not.

**1. Generate a token and set it on calmerge.**

```bash
openssl rand -hex 32          # put the result in .env as AUTH_TOKEN=...
```

The bundled `compose.yml` passes `AUTH_TOKEN` through and publishes calmerge on
`127.0.0.1:8076` (loopback only) so the host's `cloudflared` can reach it without
exposing it on the LAN.

**2. Point your host `cloudflared` at it.** Add an ingress rule mapping a public
hostname to the loopback origin (in `~/.cloudflared/config.yml` for a locally
managed tunnel, or as a public hostname in the Zero Trust dashboard for a
token-managed one):

```yaml
# ~/.cloudflared/config.yml
tunnel: <your-tunnel-id>
credentials-file: /root/.cloudflared/<your-tunnel-id>.json
ingress:
  - hostname: calmerge.example.com
    service: http://localhost:8076
  - service: http_status:404
```

`cloudflared` adds the `Cf-Connecting-Ip` header on every proxied request, which
is exactly what makes calmerge require the token on this path.

**3. Call it from the skill.** A single authenticated request returns the merged,
day-grouped schedule:

```bash
curl -s -H "Authorization: Bearer $AUTH_TOKEN" \
  "https://calmerge.example.com/events?days=2" | jq '.days'
```

A wrong/missing token on the tunnel path returns `401 {"error":"unauthorized"}`.
For an extra layer you can also put Cloudflare Access in front of the hostname,
but the app-level token is sufficient on its own.

## MCP endpoint

`/mcp` exposes calmerge as an [MCP](https://modelcontextprotocol.io)
server over Streamable HTTP. It serves from the same in-memory cache as
`/events`, so tool calls never hit the upstream feeds.

**New tools show up without restarting the client.** The server keeps
sessions (idle ones close after a week) and advertises `tools.listChanged`:

- A deploy restarts calmerge and drops every session. The client's next
  request gets a `404`, and the spec has it re-initialize, which re-reads the
  tool list.
- A tool added while calmerge is running is announced with
  `notifications/tools/list_changed`, on the session's `GET` event stream
  (2025-06-18 protocol) or a `subscriptions/listen` stream (newer, sessionless
  clients).

A client has to handle that `404` by re-initializing (the spec requires it).
One that doesn't will fail after every restart until it reconnects; rejected
requests are logged (`POST /mcp -> 404 (... mcp-session=true)`), so that case
is easy to spot.

Stateless mode would break the first path and the older-protocol half of
the second, so tests cover both. Whether a given client acts on the signal is
up to the client.

**Auth** is the same as `/events`: internal requests need nothing, tunnel
requests (any `Cf-Connecting-Ip` / `Cf-Ray` header) must send
`Authorization: Bearer <AUTH_TOKEN>` or get `401`. The SDK's DNS-rebinding
guard is disabled because `cloudflared` reaches the loopback origin with a
public `Host` header; the bearer check is the gate.

**Tool: `set_event_entity`** (needs entities configured and, always, the
bearer token): `{seriesId, entity}` saves the entity for a whole series, `entity:
"none"` resets it to automatic tagging. See [Corrections](#corrections).

**Tool: `get_calendar_events`** (`readOnlyHint: true`)

| Input | Type | Notes |
|---|---|---|
| `days` | int | Days to return, today inclusive. Default `3`. Up to `LOOKAHEAD_DAYS` + 1 (today); asking for more is capped there, with a warning. |

Returns one text content block holding JSON:

```json
{
  "generated": "2026-09-28T08:00:00-04:00",
  "updated": "2026-09-28T08:00:01-04:00",
  "warming": false,
  "count": 12,
  "days": [ { "label": "Today · Mon, Sep 28", "date": "2026-09-28", "events": [ ... ] } ],
  "errors": [],
  "warnings": []
}
```

`days[]` is the same Day/Event shape as `/events`; the flat `events[]` list is
dropped. `updated` is the last successful background refresh (absent while
warming); `warming`, `errors`, and `warnings` are also left out when
false/empty. `warnings` flags incomplete coverage the caller should surface:

- cache still warming (first refresh not done yet)
- stale data (last refresh older than 2x `CACHE_TTL_MIN`)
- one or more feeds failed to refresh, so their events may be missing or out
  of date (details in `errors`)
- requested days run past the server's `LOOKAHEAD_DAYS`

**Claude custom connector.** In Claude, go to Settings → Connectors → Add
custom connector and set:

- URL: `https://calmerge.example.com/mcp`
- Request header: `Authorization: Bearer <AUTH_TOKEN>`

Quick check from a shell:

```bash
curl -s -X POST "https://calmerge.example.com/mcp" \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'
```

## Local sanity check

```bash
export FEEDS='[{"name":"Work","url":"https://..."}]'
go run .                       # or: go build -o calmerge .
curl -s localhost:8076/events | jq '.days[0]'
```

Or install the binary straight from the module:

```bash
go install github.com/drudge/calmerge@latest
```

Dependencies are fetched from the Go module proxy and verified against `go.sum`;
the first build downloads them into the local module cache.

## License

[MIT](LICENSE). The bundled IBM Plex fonts are under the SIL Open Font
License (`ui/static/fonts/OFL.txt`).
