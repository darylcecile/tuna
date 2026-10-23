package tuna

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func mcpService(t *testing.T, svc *Service) (*mcp.ClientSession, *httptest.Server) {
	t.Helper()
	// Test names can make t.TempDir paths exceed macOS's Unix socket limit.
	dir, err := os.MkdirTemp("", "tuna-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := Paths{Dir: dir}
	l, err := net.Listen("unix", p.file("tuna.sock"))
	if err != nil {
		t.Fatal(err)
	}
	service := httptest.NewUnstartedServer(svc.handler())
	service.Listener.Close()
	service.Listener = l
	service.Start()
	t.Cleanup(service.Close)

	ct, st := mcp.NewInMemoryTransports()
	ss, err := newMCP(p, "test").Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, service
}

func requireMCPError(t *testing.T, result *mcp.CallToolResult, err error, want string) {
	t.Helper()
	if err == nil && (result == nil || !result.IsError || result.StructuredContent != nil) {
		t.Fatalf("expected tool error without queued acknowledgement: %+v", result)
	}
	b, _ := json.Marshal(result)
	if detail := fmt.Sprint(err) + string(b); !strings.Contains(detail, want) {
		t.Fatalf("expected error containing %q: %s", want, detail)
	}
}

func TestMCPRememberQueuesThenConsolidates(t *testing.T) {
	p, c, s := fixture(t)
	note := "For code reviews, keep summaries concise."
	var generated atomic.Int32
	g := fakeGenerator(func(_ string, v any) error {
		generated.Add(1)
		if _, ok := v.(*Consolidation); ok {
			return response(`{"keep_ids":[1]}`)("", v)
		}
		return response(fmt.Sprintf(`{"notes":[{"event_id":1,"rule":%q,"category":"communication","evidence":%q}]}`, note, note))("", v)
	})
	svc := &Service{p: p, c: c, s: s, g: g, ctx: t.Context()}
	cs, _ := mcpService(t, svc)
	original, err := os.ReadFile(c.AgentsFile)
	if err != nil {
		t.Fatal(err)
	}
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{
		"note": note, "harness": "copilot", "model": "provider/model", "session": "session-1",
	}})
	if err != nil || result.IsError {
		t.Fatalf("remember: %+v, %v", result, err)
	}
	b, err := json.Marshal(result.StructuredContent)
	if err != nil || string(b) != `{"queued":true}` {
		t.Fatalf("queued acknowledgement: %s, %v", b, err)
	}
	// Read through a fresh connection to verify committed queue storage.
	reader, err := openStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	events, err := reader.pending(20)
	if err != nil || len(events) != 1 {
		t.Fatalf("queued events: %+v, %v", events, err)
	}
	e := events[0]
	if e.Text != note || e.Harness != "copilot" || e.Model != "provider/model" || e.Session != "session-1" || e.Time.IsZero() {
		t.Fatalf("lost note or attribution: %+v", e)
	}
	if notes, err := reader.list(Filter{}); err != nil || len(notes) != 0 || generated.Load() != 0 {
		t.Fatalf("remember performed immediate analysis: %+v, %v", notes, err)
	}
	if b, err := os.ReadFile(c.AgentsFile); err != nil || !bytes.Equal(b, original) {
		t.Fatalf("remember changed published instructions: %s, %v", b, err)
	}

	w := httptest.NewRecorder()
	svc.handler().ServeHTTP(w, httptest.NewRequest("POST", "/consolidate", nil))
	if w.Code != 200 {
		t.Fatalf("later consolidation: %d %s", w.Code, w.Body.String())
	}
	if events, err := reader.pending(20); err != nil || len(events) != 0 {
		t.Fatalf("consolidation did not drain the queue: %+v, %v", events, err)
	}
	notes, err := reader.list(Filter{})
	if err != nil || len(notes) != 1 {
		t.Fatalf("consolidated notes: %+v, %v", notes, err)
	}
	n := notes[0]
	if n.EventID != e.ID || n.Rule != note || n.Evidence != note || n.Harness != e.Harness || n.Model != e.Model || n.Session != e.Session {
		t.Fatalf("consolidation lost remembered evidence: %+v", n)
	}
	if b, err := os.ReadFile(c.AgentsFile); err != nil || !strings.Contains(string(b), note) {
		t.Fatalf("consolidation did not publish the note: %s, %v", b, err)
	}
}

func TestMCPRememberDefaultsAndFailures(t *testing.T) {
	p, c, s := fixture(t)
	cs, service := mcpService(t, &Service{p: p, c: c, s: s, ctx: t.Context()})
	note := "Keep implementation changes focused."
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{"note": note}})
	if err != nil || result.IsError {
		t.Fatalf("remember without metadata: %+v, %v", result, err)
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing", map[string]any{}, "note"},
		{"blank", map[string]any{"note": " \n\t\u2003"}, "note must not be blank"},
		{"oversized", map[string]any{"note": strings.Repeat("é", 64*1024+1)}, "128 KiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "remember", Arguments: tc.args})
			requireMCPError(t, result, err, tc.want)
		})
	}
	if _, err := s.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	result, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{"note": "This write must fail."}})
	requireMCPError(t, result, err, "readonly")
	service.Close()
	result, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{"note": "This offline call must fail."}})
	requireMCPError(t, result, err, "cannot reach tuna")
	events, err := s.pending(20)
	if err != nil || len(events) != 1 {
		t.Fatalf("failed calls mutated the queue: %+v, %v", events, err)
	}
	e := events[0]
	if e.Text != note || e.Harness != "mcp" || e.Model != "unknown" || e.Session != "" {
		t.Fatalf("incorrect default attribution or mutated note: %+v", e)
	}
}
