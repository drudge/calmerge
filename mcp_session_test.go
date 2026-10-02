package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPToolListChangedReachesClient: a tool added while a client is
// connected shows up for that client without it reconnecting.
func TestMCPToolListChangedReachesClient(t *testing.T) {
	cfg := mcpTestConfig(t)
	server := newMCPServer(fixedConfig(cfg), storeWithDays(cfg, 1))
	srv := httptest.NewServer(mcpHTTPHandler(server))
	defer srv.Close()

	changed := make(chan struct{}, 1)
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			select {
			case changed <- struct{}{}:
			default:
			}
		},
	})
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if caps := cs.InitializeResult().Capabilities; caps.Tools == nil || !caps.Tools.ListChanged {
		t.Fatalf("server doesn't advertise tools.listChanged: %#v", caps.Tools)
	}

	mcp.AddTool(server, &mcp.Tool{Name: "brand_new_tool"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("client never got notifications/tools/list_changed")
	}

	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tl := range lt.Tools {
		found = found || tl.Name == "brand_new_tool"
	}
	if !found {
		t.Error("new tool missing from the client's next tools/list")
	}
}

// TestMCPRestartForcesReinit: a session from before a restart (deploy) gets a
// 404, which is the spec's signal for the client to re-initialize and so
// re-read the tool list. In stateless mode it would quietly keep working with
// the old list.
func TestMCPRestartForcesReinit(t *testing.T) {
	cfg := mcpTestConfig(t)
	before := newMCPHandler(fixedConfig(cfg), storeWithDays(cfg, 1))
	w := httptest.NewRecorder()
	before.ServeHTTP(w, mcpInitRequest())
	if w.Code != http.StatusOK {
		t.Fatalf("initialize: %d %s", w.Code, w.Body.String())
	}
	sid := w.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("no Mcp-Session-Id: server is still stateless")
	}

	after := newMCPHandler(fixedConfig(cfg), storeWithDays(cfg, 1)) // the redeployed process
	r := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Mcp-Session-Id", sid)
	w = httptest.NewRecorder()
	after.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("old session after restart = %d, want 404 so the client re-initializes", w.Code)
	}
}

// TestMCPListChangedOldProtocol does the same over the 2025-06-18 protocol by
// hand (initialize, then a GET event stream on the session), which is what
// clients that predate sessionless MCP rely on. Stateless mode answers that
// GET with 405, so those clients would never hear about new tools.
func TestMCPListChangedOldProtocol(t *testing.T) {
	cfg := mcpTestConfig(t)
	server := newMCPServer(fixedConfig(cfg), storeWithDays(cfg, 1))
	srv := httptest.NewServer(mcpHTTPHandler(server))
	defer srv.Close()

	post := func(sid, body string) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		if sid != "" {
			r.Header.Set("Mcp-Session-Id", sid)
			r.Header.Set("Mcp-Protocol-Version", "2025-06-18")
		}
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := post("", mcpInitBody)
	res.Body.Close()
	sid := res.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("no session id")
	}
	post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`).Body.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	get, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	get.Header.Set("Accept", "text/event-stream")
	get.Header.Set("Mcp-Session-Id", sid)
	get.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	stream, err := http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("GET event stream = %d, want 200", stream.StatusCode)
	}

	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var seen strings.Builder
		for {
			n, err := stream.Body.Read(buf)
			seen.Write(buf[:n])
			if strings.Contains(seen.String(), "notifications/tools/list_changed") {
				got <- seen.String()
				return
			}
			if err != nil {
				return
			}
		}
	}()

	time.Sleep(50 * time.Millisecond) // let the stream attach before the change
	mcp.AddTool(server, &mcp.Tool{Name: "brand_new_tool"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("no notifications/tools/list_changed on the session's event stream")
	}
}
