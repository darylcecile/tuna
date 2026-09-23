package tuna

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeGenerator func(string, any) error

func TestMain(m *testing.M) {
	if os.Getenv("TUNA_TEST_GENERATOR") == "1" {
		b, _ := json.Marshal(map[string]string{"directory": os.Getenv("PWD"), "internal": os.Getenv("TUNA_INTERNAL")})
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "assistant.message", "data": map[string]string{"content": string(b)}})
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func (f fakeGenerator) Generate(_ context.Context, prompt string, result any) error {
	return f(prompt, result)
}
func response(s string) fakeGenerator {
	return func(_ string, v any) error { return json.Unmarshal([]byte(s), v) }
}

func fixture(t *testing.T) (Paths, Config, *Store) {
	t.Helper()
	home := t.TempDir()
	p := Paths{Home: home, Dir: filepath.Join(home, "data")}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("COPILOT_HOME", filepath.Join(home, ".copilot"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	c, err := p.config()
	if err != nil {
		t.Fatal(err)
	}
	s, err := openStore(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := atomicWrite(c.AgentsFile, []byte("# My rules\n\nKeep commits focused.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return p, c, s
}

func seed(t *testing.T, s *Store, text, rule, category string, at time.Time) int64 {
	t.Helper()
	err := s.ingest(Event{Harness: "opencode", Session: "session-1", Model: "provider/model", Text: text, Time: at})
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.pending(20)
	if err != nil {
		t.Fatal(err)
	}
	e := events[len(events)-1]
	err = s.saveAnalysis([]Event{e}, []Note{{EventID: e.ID, Rule: rule, Category: category, Evidence: text, Harness: e.Harness, Model: e.Model, Session: e.Session, Created: e.Time}})
	if err != nil {
		t.Fatal(err)
	}
	notes, err := s.list(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return notes[0].ID
}

func TestAnalysisEvidenceAndQueue(t *testing.T) {
	_, _, s := fixture(t)
	e := Event{Key: "msg-1", Harness: "opencode", Session: "ses-1", Model: "m", Text: "No, stop writing so many tests. Only test changed behavior.", Time: time.Now()}
	for i := 0; i < 2; i++ {
		if err := s.ingest(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ingest(Event{Key: "msg-2", Harness: "opencode", Text: "Add a settings screen.", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	events, _ := s.pending(20)
	if len(events) != 2 {
		t.Fatalf("duplicate capture queued: %d", len(events))
	}
	bad := response(fmt.Sprintf(`{"notes":[{"event_id":%d,"rule":"Never test.","category":"testing","evidence":"invented quote"}]}`, events[0].ID))
	if _, err := analyze(context.Background(), s, bad); err == nil {
		t.Fatal("accepted invented evidence")
	}
	if pending, _ := s.pending(20); len(pending) != 2 {
		t.Fatal("failed analysis lost pending events")
	}
	good := response(fmt.Sprintf(`{"notes":[{"event_id":%d,"rule":"Test only changed behavior.","category":"testing","evidence":"Only test changed behavior."}]}`, events[0].ID))
	n, err := analyze(context.Background(), s, good)
	if err != nil || n != 1 {
		t.Fatalf("analyze = %d, %v", n, err)
	}
	notes, err := s.list(Filter{Category: "testing", Model: "m"})
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes=%v, err=%v", notes, err)
	}
	if notes[0].Session != "ses-1" {
		t.Fatal("lost attribution")
	}
	var retained int
	if err := s.QueryRow("SELECT count(*) FROM events WHERE payload!='{}' OR processed=0").Scan(&retained); err != nil || retained != 0 {
		t.Fatalf("raw payload not cleared: %d %v", retained, err)
	}
}

func TestConsolidateConflictsExpiryAndRemoval(t *testing.T) {
	p, c, s := fixture(t)
	old := seed(t, s, "Give me detailed answers.", "Give detailed answers.", "communication", time.Now().Add(-time.Hour))
	latest := seed(t, s, "Stop the long responses, keep it short.", "Keep responses short.", "communication", time.Now())
	stale := seed(t, s, "Use the old workflow.", "Use the old workflow.", "workflow", time.Now().AddDate(0, 0, -200))
	if err := atomicWrite(c.AgentsFile, []byte("# My rules\n\nKeep commits focused.\nGive detailed answers.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(p.Home, "AGENTS.md")
	if err := os.Symlink(c.AgentsFile, link); err != nil {
		t.Fatal(err)
	}
	g := response(fmt.Sprintf(`{"keep_ids":[%d],"edits":[{"old":"Give detailed answers.\n","new":"","source_id":%d}]}`, latest, latest))
	n, err := consolidate(context.Background(), p, c, s, g)
	if err != nil || n != 1 {
		t.Fatalf("consolidate %d %v", n, err)
	}
	b, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Keep commits focused.") || !strings.Contains(string(b), "Keep responses short.") || strings.Contains(string(b), "Give detailed") {
		t.Fatalf("incorrect instructions:\n%s", b)
	}
	info, _ := os.Lstat(link)
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("lost symlink")
	}
	notes, _ := s.list(Filter{All: true})
	statuses := map[int64]string{}
	for _, n := range notes {
		statuses[n.ID] = n.Status
	}
	if statuses[old] != "superseded" || statuses[stale] != "stale" || statuses[latest] != "active" {
		t.Fatalf("statuses=%v", statuses)
	}
	if err := removeNotes(p, c, s, []int64{9999}); err == nil {
		t.Fatal("unknown ID accepted")
	}
	if err := removeNotes(p, c, s, []int64{latest}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(link)
	if strings.Contains(string(b), "Keep responses short.") {
		t.Fatal("removed preference still published")
	}
	backups, _ := filepath.Glob(p.file("backups/*.md"))
	if len(backups) < 2 {
		t.Fatal("missing original backups")
	}
	notes, _ = s.list(Filter{})
	if len(notes) != 0 {
		t.Fatal("removed note still queried")
	}
}

func TestConsolidationRejectsUnknownIDsAndConcurrentEdit(t *testing.T) {
	p, c, s := fixture(t)
	id := seed(t, s, "Stop adding unrelated changes.", "Keep changes focused.", "scope", time.Now())
	if _, err := consolidate(context.Background(), p, c, s, response(`{"keep_ids":[9999]}`)); err == nil {
		t.Fatal("accepted fabricated ID")
	}
	g := fakeGenerator(func(_ string, v any) error {
		if err := os.WriteFile(c.AgentsFile, []byte("User edited this while analysis ran.\n"), 0600); err != nil {
			return err
		}
		return json.Unmarshal([]byte(fmt.Sprintf(`{"keep_ids":[%d]}`, id)), v)
	})
	if _, err := consolidate(context.Background(), p, c, s, g); err == nil || !strings.Contains(err.Error(), "changed during") {
		t.Fatalf("expected concurrent edit protection: %v", err)
	}
	b, _ := os.ReadFile(c.AgentsFile)
	if string(b) != "User edited this while analysis ran.\n" {
		t.Fatal("overwrote user edit")
	}
}

func TestSetupPreservesSettingsAndIsRepeatable(t *testing.T) {
	p, c, _ := fixture(t)
	for _, h := range harnesses {
		dir := harnessDir(p, h)
		name := "AGENTS.md"
		if h == "claude" {
			name = "CLAUDE.md"
		}
		if h == "copilot" {
			name = "copilot-instructions.md"
		}
		if err := atomicWrite(filepath.Join(dir, name), []byte("Existing "+h+" instruction.\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	claudeSettings := filepath.Join(harnessDir(p, "claude"), "settings.json")
	if err := atomicWrite(claudeSettings, []byte(`{"model":"keep-me","hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"my-hook"}]}]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	codexConfig := filepath.Join(harnessDir(p, "codex"), "config.toml")
	original := "# Keep this comment\nmodel = 'keep-me'\n[mcp_servers]\n[mcp_servers.other]\ncommand = 'other'\n"
	if err := atomicWrite(codexConfig, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	for _, h := range harnesses {
		for i := 0; i < 2; i++ {
			if _, err := setup(p, c, h); err != nil {
				t.Fatalf("%s: %v", h, err)
			}
		}
	}
	b, _ := os.ReadFile(c.AgentsFile)
	for _, h := range harnesses {
		if strings.Count(string(b), "Existing "+h+" instruction.") != 1 {
			t.Fatalf("instruction lost or duplicated: %s", b)
		}
	}
	var settings map[string]any
	b, _ = os.ReadFile(claudeSettings)
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	groups := settings["hooks"].(map[string]any)["UserPromptSubmit"].([]any)
	if settings["model"] != "keep-me" || len(groups) != 2 {
		t.Fatalf("overwrote settings or duplicated hook: %s", b)
	}
	b, _ = os.ReadFile(codexConfig)
	if !strings.Contains(string(b), original) || strings.Count(string(b), "tuna:mcp:start") != 1 {
		t.Fatalf("modified unrelated TOML: %s", b)
	}
	b, _ = os.ReadFile(filepath.Join(harnessDir(p, "opencode"), "plugins", "tuna", "index.js"))
	if strings.Contains(string(b), "__TUNA_") {
		t.Fatal("plugin placeholders unresolved")
	}
	for _, h := range harnesses {
		name := "AGENTS.md"
		if h == "claude" {
			name = "CLAUDE.md"
		}
		if h == "copilot" {
			name = "copilot-instructions.md"
		}
		target, err := filepath.EvalSymlinks(filepath.Join(harnessDir(p, h), name))
		canonical, _ := filepath.EvalSymlinks(c.AgentsFile)
		if err != nil || target != canonical {
			t.Fatalf("%s link %s %v", h, target, err)
		}
	}
}

func TestSetupRejectsMalformedConfig(t *testing.T) {
	p, _, _ := fixture(t)
	path := filepath.Join(harnessDir(p, "claude"), "settings.json")
	bad := []byte("{broken")
	if err := atomicWrite(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if err := installHarness(p, "claude", harnessDir(p, "claude"), "/path/tuna"); err == nil {
		t.Fatal("malformed config overwritten")
	}
	b, _ := os.ReadFile(path)
	if !bytes.Equal(b, bad) {
		t.Fatal("changed malformed config")
	}
}

func TestNormalizeCapture(t *testing.T) {
	for _, tc := range []struct{ h, json string }{
		{"opencode", `{"prompt":"No, keep it short.","session_id":"s","model":"p/m","timestamp":"2026-09-23T10:00:00Z","key":"msg_1"}`},
		{"claude", `{"prompt":"No, keep it short.","session_id":"s","model":"p/m"}`},
		{"codex", `{"prompt":"No, keep it short.","session_id":"s","model":"p/m","turn_id":"turn_1"}`},
		{"copilot", `{"prompt":"No, keep it short.","sessionId":"s","timestamp":1790157600000}`},
	} {
		t.Run(tc.h, func(t *testing.T) {
			e, err := normalizeEvent(tc.h, []byte(tc.json))
			if err != nil {
				t.Fatal(err)
			}
			if e.Session != "s" || e.Text != "No, keep it short." || e.Model == "" {
				t.Fatalf("incorrect normalization: %+v", e)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte("{partial\n"+`{"type":"assistant","message":{"role":"assistant","model":"claude-test","content":[{"type":"text","text":"I added a large abstraction."}]}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"prompt": "No, simplify.", "transcript_path": path})
	e, err := normalizeEvent("claude", payload)
	if err != nil || e.Model != "claude-test" || e.Context != "I added a large abstraction." {
		t.Fatalf("transcript metadata=%+v, %v", e, err)
	}
	for _, tc := range []struct{ log, model string }{
		{`{"type":"turn_context","payload":{"model":"codex-test"}}` + "\n" + `{"type":"response_item","payload":{"role":"assistant","content":[{"type":"output_text","text":"Previous answer."}]}}`, "codex-test"},
		{`{"type":"assistant.message","data":{"model":"copilot-test","content":"Previous answer."}}`, "copilot-test"},
	} {
		if err := os.WriteFile(path, []byte(tc.log+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		model, text := transcriptContext(path)
		if model != tc.model || text != "Previous answer." {
			t.Fatalf("transcript context = %q, %q", model, text)
		}
	}
}

func TestServiceRoutesAndMCP(t *testing.T) {
	p, c, s := fixture(t)
	id := seed(t, s, "Stop changing unrelated code.", "Keep edits focused.", "scope", time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := &Service{p: p, c: c, s: s, g: response(fmt.Sprintf(`{"keep_ids":[%d]}`, id)), ctx: ctx, cancel: cancel, version: "test"}
	h := svc.handler()
	req := httptest.NewRequest("POST", "/query", strings.NewReader(`{"category":"scope"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Keep edits focused.") {
		t.Fatalf("query response %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest("POST", "/consolidate", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("consolidate response: %s", w.Body.String())
	}
	server := newMCP(p, "test")
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "preferences", Arguments: map[string]any{"category": "scope"}})
	if err != nil || result.IsError {
		t.Fatalf("MCP query: %+v %v", result, err)
	}
	b, _ := json.Marshal(result)
	if !bytes.Contains(b, []byte("Keep edits focused.")) {
		t.Fatalf("MCP lost results: %s", b)
	}
	result, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list", Arguments: map[string]any{"when": "nonsense"}})
	if err == nil && !result.IsError {
		t.Fatal("MCP accepted invalid date")
	}
}

func TestDatesAndSchedule(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 8, 23, 30, 0, 0, loc)
	start, end, err := dateRange("today", now)
	if err != nil {
		t.Fatal(err)
	}
	if end.Sub(start) != 23*time.Hour {
		t.Fatal("date window mishandles DST")
	}
	start, end, err = dateRange("week ago", now)
	if err != nil || start.Day() != 1 || !end.Equal(now) {
		t.Fatalf("week range %v %v %v", start, end, err)
	}
	if !consolidationDue(now, "23:00", "") || consolidationDue(now, "23:00", "2026-03-08") || consolidationDue(now, "23:45", "") {
		t.Fatal("wrong daily schedule")
	}
}

func TestDateFilterIncludesFractionalStart(t *testing.T) {
	_, _, s := fixture(t)
	start := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	seed(t, s, "No, focus.", "Keep changes focused.", "scope", start.Add(time.Millisecond))
	seed(t, s, "Stop the long answers.", "Keep answers short.", "communication", start.AddDate(0, 0, 1))
	notes, err := s.list(Filter{Since: start, Until: start.AddDate(0, 0, 1)})
	if err != nil || len(notes) != 1 || notes[0].Category != "scope" {
		t.Fatalf("incorrect day boundary: %+v %v", notes, err)
	}
}

func TestCLIQueryJSONAndHelp(t *testing.T) {
	p, _, s := fixture(t)
	seed(t, s, "No, short answers please.", "Keep answers short.", "communication", time.Now())
	for _, args := range [][]string{{"--home", p.Dir, "query", "short", "--json"}, {"help", "setup"}} {
		cmd := newCommand("test")
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if args[0] != "help" {
			var notes []Note
			if err := json.Unmarshal(out.Bytes(), &notes); err != nil || len(notes) != 1 {
				t.Fatalf("query JSON %s %v", out.Bytes(), err)
			}
		} else if !strings.Contains(out.String(), "--analyzer") {
			t.Fatal("missing setup help")
		}
	}
}

func TestGeneratorOutputParsingAndChecksum(t *testing.T) {
	b, err := opencodeText([]byte("{\"type\":\"step_start\"}\n{\"type\":\"text\",\"part\":{\"text\":\"{\\\"notes\\\":[]}\"}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Notes []Finding `json:"notes"`
	}
	if err := decodeObject(b, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := opencodeText([]byte(`{"type":"error","error":{"message":"Sign in to your model provider"}}`)); err == nil || !strings.Contains(err.Error(), "Sign in") {
		t.Fatalf("lost actionable harness error: %v", err)
	}
	if err := decodeObject([]byte("```json\n{\"notes\":[]}\n```"), &output); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksum([]byte("hello"), "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824  tuna_test", "tuna_test"); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksum([]byte("tampered"), "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824  tuna_test", "tuna_test"); err == nil {
		t.Fatal("accepted bad checksum")
	}
	b, err = copilotText([]byte("{\"type\":\"model.call_start\"}\n" + `{"type":"assistant.message","data":{"content":"{\"notes\":[]}"}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeObject(b, &output); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessGeneratorUsesIsolatedWorkingDirectory(t *testing.T) {
	p, c, _ := fixture(t)
	bin := filepath.Join(p.Home, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(bin, "copilot")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PWD", p.Home)
	t.Setenv("TUNA_TEST_GENERATOR", "1")
	c.Analyzer = "copilot"
	var result struct{ Directory, Internal string }
	if err := (HarnessGenerator{p, c}).Generate(context.Background(), "Return JSON.", &result); err != nil {
		t.Fatal(err)
	}
	actual, _ := filepath.EvalSymlinks(result.Directory)
	expected, _ := filepath.EvalSymlinks(p.file("analysis"))
	if actual != expected || result.Internal != "1" {
		t.Fatalf("analyzer inherited caller context: %+v", result)
	}
}
