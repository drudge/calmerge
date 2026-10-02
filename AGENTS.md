# AGENTS.md

Guidance for coding agents working on calmerge. The README covers what it does
and how to run it; this file covers how to change it safely.

## What this is

A small Go service (one `main` package, no subpackages) that fetches ICS feeds,
merges them into day-grouped JSON for a Glance widget (`/events`), serves the
same data over MCP (`/mcp`) for AI assistants (e.g. a daily-briefing Claude
skill), and tags each event with the business/client it belongs to so a
briefing can map meetings to a work plan.

## Layout

| File | What lives there |
|---|---|
| `main.go` | Types (`Event`, `Response`, `config`), ICS fetch/parse, description and attendee cleanup, the store, the refresh loop, HTTP handlers, tunnel auth. |
| `config.go` | TOML config file + env fallback (`loadConfig`), hot reload (`watchConfig`), `-print-config` / `-env-file`. |
| `classify.go` | Entity tagging: rules, naive Bayes, feature extraction. |
| `corrections.go` | User corrections (seriesId -> entity), saved to `/data/corrections.json`; shared `writeJSONAtomic`. |
| `lessons.go` | Classifier long-term memory, saved to `/data/lessons.json`. |
| `mcp.go` | MCP server: `get_calendar_events` (read-only) and `set_event_entity` (write). |
| `ui.go` | Review UI: routes, sign-in (cookie derived from `AUTH_TOKEN`, guesses rate-limited), handlers, embedded assets. |
| `ui_view.go` | View models: agenda (gaps, folded today, time off), week grid layout, details panel, status. |
| `ui/templates/*.html` | Go templates (`layout.html` shell/status/toast/login, `views.html` everything that swaps). |
| `ui/tailwind.css` → `ui/static/app.css` | Tailwind v4 source and its committed build; also all keyframes and view-transition CSS. |
| `ui/static/` | Vendored htmx 4.0.0 + hx-live (checksum-verified), `app.js` (tiny helpers), favicon, `fonts.css` + `fonts/` (IBM Plex woff2 from Fontsource, OFL). |
| `devseed.go` | `-seed file.json`: serve a saved `/events` payload instead of fetching feeds (UI work). |
| `calmerge.example.toml` | Template config with every key and a sample entity list. Keep it in sync with `fileConfig`. |

## Commands

```bash
go build ./...
go vet ./...
go test -race ./...
```

Run all three before calling anything done; `gofmt` must be clean. After
touching `ui/templates` or `ui/tailwind.css`, rebuild the CSS with
`go generate ./...` (needs the standalone `tailwindcss` v4 binary on PATH) and
commit `ui/static/app.css`. For UI work, `-seed` a saved `/events` payload
(README, Review UI) and look at the page in a real browser. Docker
builds from the `Dockerfile` (distroless, nonroot). To try the real server
locally, point `CONFIG_FILE` at a scratch TOML, set `LISTEN` and `AUTH_TOKEN`,
and `go run .`.

## Conventions

- Match the existing style: doc comments on every function and non-obvious
  var, explaining *why* (feed quirks, spec rules), not restating the code.
- Table-driven tests where cases vary by input. Tests use real ICS text and
  real-shaped data (published Outlook feeds, forwarded Google invites).
- New tunables go in `fileConfig` (TOML key) *and* keep an env var fallback,
  then get documented in the README config table and `calmerge.example.toml`.
- New optional `Event` fields are `omitempty` so the Glance widget payload
  stays lean.
- Nil-safe receivers are the pattern for optional parts (`*classifier`,
  `*corrections`, `*lessons`): a nil value means "feature off", not a crash.

## Things that bite

- **Outlook published feeds strip a lot.** No `ATTENDEE`, no `ORGANIZER`, no
  `CATEGORIES`, and Windows time zone names (remapped in `remapWindowsTZ`).
  Attendees mostly come from Google "Guests" rosters in `DESCRIPTION`. Don't
  build features that assume structured attendees or categories exist.
- **Microsoft Graph is out of scope.** calmerge works from published ICS
  feeds only, for people who can't get API access. Don't propose it.
- **MCP must stay stateful** (sessions, not `Stateless: true`). Sessions are
  how clients pick up new tools without restarting: a deploy drops sessions,
  clients get a 404 and re-initialize; live changes go out as
  `notifications/tools/list_changed`. `mcp_session_test.go` fails if this
  regresses.
- **Writes need the bearer token, always.** Reads on `/mcp` and `/events` are
  open to internal (non-Cloudflare) traffic unless `require_auth` is on;
  `set_event_entity` checks the token itself even then. Keep any new write
  path behind the same check, and any new read route behind
  `requireTunnelAuth`.
- **`/config` and `/data` are separate mounts.** Config is a file mount at
  `/config/calmerge.toml`; `/data` is a volume for corrections and lessons.
  Putting the config inside `/data` gets it hidden by the volume.
- **Secrets:** feed URLs (in `FEEDS` / `[[feeds]]`) and `AUTH_TOKEN` are
  secrets. Never log them, never print them in replies, and never let one
  into an error that reaches `/events` or `/mcp` (see `hideURL`). Example
  files and tests use made-up names and `example` domains only: no real
  people, clients, feed URLs or tenant ids. `AUTH_TOKEN` stays
  env-only and `-print-config` leaves it out. The real `calmerge.toml` is
  git-ignored.
- **Distroless has no shell.** A container terminal (Dokploy, Portainer)
  can't open; use `docker exec <container> /calmerge -print-config` on the
  host.
- **Classifier state:** the model retrains from scratch every refresh (from
  firm labels in the window plus remembered lessons). Only firm labels
  (`manual`, `category`, `rule`) are ever trained on or remembered; never
  train on `learned` or `feed` guesses, or it reinforces its own mistakes.

- **UI: rebuild CSS or classes silently don't exist.** Tailwind only emits
  classes it finds in `ui/templates`; a class added without rebuilding does
  nothing.
- **UI: layer order beats specificity.** Rules in `@layer components` lose
  to any utility on the same element (e.g. a `display: none` state rule vs a
  `flex` utility). State rules that must win go unlayered.
- **UI: view-transition names must be unique on the page** at capture time,
  or the whole transition aborts. Only ever name one element per name.
- **UI: morphs keep elements by `id`.** Rows, blocks and the folded-today box
  have stable ids so htmx's morph updates them in place (only new or changed
  things animate). Anything that should animate open/closed must stay the
  same element (see `#earlier`).
- **UI: SVG `<pattern>` defs live once in the layout.** A `url(#…)` pointing
  into a collapsed/hidden subtree draws nothing in Chrome.
- **UI: static assets are fingerprinted** (`asset` template func). Always
  link through it, or browsers keep a stale copy for a day.
- **UI: no outside requests.** Fonts, scripts and styles all come from the
  binary. Don't add a CDN link; vendor the file under `ui/static/`.
- **UI writes** go through `applyCorrection` (shared with MCP) and require the
  session cookie *and* the `HX-Request` header.

## Deploying

Nothing in the repo deploys itself: a push to `main` builds no image and
restarts nothing. Whoever runs an instance redeploys it by hand (or wires up
their own webhook), then checks the logs and the live feed
(`get_calendar_events`, or `/events`).
