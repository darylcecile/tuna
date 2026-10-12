package tuna

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func support(t *testing.T, s *Store, id int64, event Event) Event {
	t.Helper()
	if err := s.ingest(event); err != nil {
		t.Fatal(err)
	}
	events, err := s.pending(20)
	if err != nil || len(events) != 1 {
		t.Fatalf("pending feedback = %+v, %v", events, err)
	}
	e := events[0]
	if err := s.saveAnalysis(events, []Note{{ID: id, EventID: e.ID, Evidence: e.Text, Harness: e.Harness, Model: e.Model, Session: e.Session, Created: e.Time}}); err != nil {
		t.Fatal(err)
	}
	return e
}

func historyNote(t *testing.T, s *Store, id int64) Note {
	t.Helper()
	notes, err := s.list(Filter{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("missing preference %d", id)
	return Note{}
}

func TestHistoryAnalysisAcrossDatedSessions(t *testing.T) {
	p, _, s := fixture(t)
	first := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	original := "When I ask for implementation, follow through instead of just explaining."
	id := seed(t, s, original, "Follow through when implementation is requested.", "workflow", first)
	before := historyNote(t, s, id)
	texts := []string{
		"I've told you ten times across different sessions: when I ask you to implement, do it instead of stopping at explanations.",
		"You keep answering rhetorical questions instead of implementing what I asked for. Follow through on implementation.",
	}
	calls := 0
	for i, text := range texts {
		at := first.AddDate(0, 0, i+2)
		e := Event{Key: fmt.Sprintf("feedback-%d", i), Text: text, Harness: "claude", Model: "other/model", Session: fmt.Sprintf("later-%d", i), Time: at}
		for range 2 {
			if err := s.ingest(e); err != nil {
				t.Fatal(err)
			}
		}
		g := fakeGenerator(func(prompt string, output any) error {
			calls++
			var input struct {
				Events      []Event `json:"events"`
				Preferences []Note  `json:"preferences"`
			}
			if err := json.Unmarshal([]byte(prompt[strings.LastIndex(prompt, "\nData:\n")+len("\nData:\n"):]), &input); err != nil {
				return err
			}
			if len(input.Events) != 1 || len(input.Preferences) != 1 || input.Preferences[0].ID != id || input.Preferences[0].Confirmations != i+1 || len(input.Preferences[0].History) != i+1 {
				t.Fatalf("analysis lost accumulated history: %+v", input)
			}
			if i > 0 && input.Preferences[0].History[1].Evidence != texts[0] {
				t.Fatal("later analysis did not receive the earlier retrospective correction")
			}
			finding := Finding{EventID: input.Events[0].ID, PreferenceID: id, Rule: before.Rule, Category: "workflow", Evidence: text}
			duplicate := finding
			duplicate.Rule = "Proceed with requested implementation."
			b, err := json.Marshal(map[string]any{"notes": []Finding{finding, duplicate}})
			if err != nil {
				return err
			}
			return json.Unmarshal(b, output)
		})
		if n, err := analyze(context.Background(), s, g); err != nil || n != 1 {
			t.Fatalf("reinforcement = %d, %v", n, err)
		}
		n := historyNote(t, s, id)
		if n.EventID != before.EventID || n.Evidence != original || n.Harness != before.Harness || n.Model != before.Model || n.Session != before.Session || !n.Created.Equal(first) || n.Rule != before.Rule {
			t.Fatalf("reinforcement changed original identity/attribution: %+v", n)
		}
		if n.Confirmations != i+2 || n.SessionCount != i+2 || !n.LastConfirmed.Equal(at) || n.History[i+1].Evidence != text || n.History[i+1].Model != e.Model {
			t.Fatalf("incorrect support statistics/history: %+v", n)
		}
	}
	if calls != 2 {
		t.Fatalf("used %d provider calls for two batches", calls)
	}
	want := historyNote(t, s, id)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := historyNote(t, reopened, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("restart lost feedback: %+v; want %+v", got, want)
	}
	var retained int
	if err := reopened.QueryRow("SELECT count(*) FROM events WHERE payload!='{}' OR processed=0").Scan(&retained); err != nil || retained != 0 {
		t.Fatalf("raw event payloads retained: %d, %v", retained, err)
	}

	// Exercise the existing MCP response, including its generated output schema.
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	server, err := newMCP(p, "test").Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "preferences", Arguments: map[string]any{"model": "other/model"}})
	if err != nil || result.IsError {
		t.Fatalf("MCP history lookup: %+v, %v", result, err)
	}
	b, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output QueryOutput
	if err := json.Unmarshal(b, &output); err != nil || len(output.Preferences) != 1 || !reflect.DeepEqual(output.Preferences[0], want) {
		t.Fatalf("MCP omitted history/statistics: %s, %v", b, err)
	}
}

func TestHistoryLegacyBackfillAndForgetting(t *testing.T) {
	p, c, s := fixture(t)
	at := time.Now().UTC().AddDate(0, 0, -3)
	id := seed(t, s, "Long introductions overwhelm me; please answer briefly.", "Keep replies concise.", "communication", at)
	quote := "please answer briefly."
	result, err := s.Exec(`INSERT INTO notes(event_id,rule,category,evidence,harness,model,session,created) SELECT event_id,'Keep replies short.',category,?,harness,model,session,created FROM notes WHERE id=?`, quote, id)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	removed := seed(t, s, "Use the legacy helper for tests.", "Use the legacy helper.", "testing", at.Add(time.Hour))
	if err := removeNotes(p, c, s, []int64{removed}); err != nil {
		t.Fatal(err)
	}
	// This is the pre-history database layout, with existing retained and forgotten notes.
	if _, err := s.Exec("DROP TABLE observations; DELETE FROM state WHERE key='history_backfilled'"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		db, err := openStore(p)
		if err != nil {
			t.Fatal(err)
		}
		n := historyNote(t, db, id)
		if n.Confirmations != 1 || n.SessionCount != 1 || len(n.History) != 1 || n.History[0].EventID != n.EventID || n.History[0].Evidence != n.Evidence || !n.LastConfirmed.Equal(at) {
			t.Fatalf("backfill lost or duplicated evidence: %+v", n)
		}
		other := historyNote(t, db, duplicate)
		if other.EventID != n.EventID || other.Confirmations != 1 || len(other.History) != 1 || other.History[0].Evidence != quote {
			t.Fatalf("backfill lost another quote from the same event: %+v", other)
		}
		forgotten := historyNote(t, db, removed)
		if forgotten.Confirmations != 0 || len(forgotten.History) != 0 || forgotten.Evidence != "" {
			t.Fatalf("backfill revived forgotten evidence: %+v", forgotten)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	s, err = openStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := consolidate(context.Background(), p, c, s, response(fmt.Sprintf(`{"keep_ids":[%d],"merges":[{"from_id":%d,"into_id":%d}]}`, id, duplicate, id))); err != nil {
		t.Fatal(err)
	}
	if got := historyNote(t, s, id); got.Confirmations != 1 || len(got.History) != 2 {
		t.Fatalf("merging legacy quotes lost evidence or counted one message twice: %+v", got)
	}
	support(t, s, id, Event{Text: "Across sessions, those lengthy explanations are still too much. Keep them short.", Harness: "claude", Model: "m2", Session: "later", Time: at.AddDate(0, 0, 2)})
	quotes := historyNote(t, s, id).History
	if err := removeNotes(p, c, s, []int64{id}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = openStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n := historyNote(t, s, id)
	if n.Status != "removed" || n.Evidence != "" || len(n.History) != 0 || n.Confirmations != 0 || n.SessionCount != 0 || !n.LastConfirmed.IsZero() {
		t.Fatalf("removal retained supporting feedback: %+v", n)
	}
	all, err := s.list(Filter{All: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(all)
	for _, o := range quotes {
		if bytes.Contains(b, []byte(o.Evidence)) {
			t.Fatalf("forgotten quotation still retrievable: %s", b)
		}
		found, err := s.list(Filter{All: true, Query: o.Evidence})
		if err != nil || len(found) != 0 {
			t.Fatalf("forgotten quotation still searchable: %+v, %v", found, err)
		}
	}
	retained, err := s.list(Filter{All: true, Query: "replies concise", Model: "provider/model", Since: at.Add(-time.Minute), Until: at.Add(time.Minute)})
	if err != nil || len(retained) != 1 || retained[0].ID != id {
		t.Fatalf("could not browse forgotten identity by its original attribution: %+v, %v", retained, err)
	}
}

func TestHistoryInvalidTargetsDoNotConsumeQueueAndRetriesDoNotCount(t *testing.T) {
	p, c, s := fixture(t)
	at := time.Now().UTC()
	id := seed(t, s, "Keep edits scoped.", "Keep edits focused.", "scope", at)
	removed := seed(t, s, "Always explain first.", "Explain first.", "workflow", at.Add(time.Second))
	if err := removeNotes(p, c, s, []int64{removed}); err != nil {
		t.Fatal(err)
	}
	if err := s.ingest(Event{Text: "Again, avoid unrelated edits.", Harness: "opencode", Session: "later", Time: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	events, _ := s.pending(20)
	e := events[0]
	n := Note{ID: id, EventID: e.ID, Rule: "Keep edits focused.", Category: "scope", Evidence: e.Text, Harness: e.Harness, Model: e.Model, Session: e.Session, Created: e.Time}
	for _, target := range []int64{9999, -1, removed} {
		finding := Finding{EventID: e.ID, PreferenceID: target, Rule: n.Rule, Category: n.Category, Evidence: n.Evidence}
		b, _ := json.Marshal(map[string]any{"notes": []Finding{finding}})
		if _, err := analyze(context.Background(), s, response(string(b))); err == nil {
			t.Fatalf("analysis accepted inactive target %d", target)
		}
		bad := n
		bad.ID = target
		if err := s.saveAnalysis(events, []Note{n, bad}); err == nil {
			t.Fatalf("save accepted inactive target %d", target)
		}
		pending, err := s.pending(20)
		if err != nil || len(pending) != 1 || historyNote(t, s, id).Confirmations != 1 {
			t.Fatalf("invalid target consumed queue or partially saved: %+v, %v", pending, err)
		}
	}
	for _, status := range []string{"stale", "superseded"} {
		if _, err := s.Exec("UPDATE notes SET status=? WHERE id=?", status, removed); err != nil {
			t.Fatal(err)
		}
		bad := n
		bad.ID = removed
		if err := s.saveAnalysis(events, []Note{bad}); err == nil {
			t.Fatalf("revived a %s preference", status)
		}
	}
	for range 2 {
		if err := s.saveAnalysis(events, []Note{n, n}); err != nil {
			t.Fatal(err)
		}
	}
	if got := historyNote(t, s, id); got.Confirmations != 2 || len(got.History) != 2 {
		t.Fatalf("duplicate findings/retry inflated support: %+v", got)
	}
	// Retrying a successful new-preference save must not create another identity either.
	n.ID = 0
	if err := s.saveAnalysis(events, []Note{n}); err != nil {
		t.Fatal(err)
	}
	active, err := s.list(Filter{})
	if err != nil || len(active) != 1 {
		t.Fatalf("retry recreated processed feedback: %+v, %v", active, err)
	}
}

func TestHistoryEquivalentNewFindingsShareOnePreference(t *testing.T) {
	_, _, s := fixture(t)
	at := time.Now().UTC()
	for i, session := range []string{"same", "same", ""} {
		if err := s.ingest(Event{Text: fmt.Sprintf("Correction %d: use short replies.", i), Harness: "opencode", Session: session, Time: at.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	events, _ := s.pending(20)
	findings := []Finding{}
	for _, e := range events {
		findings = append(findings, Finding{EventID: e.ID, Rule: "Keep replies concise.", Category: "communication", Evidence: e.Text})
	}
	findings = append(findings, findings[0])
	findings[len(findings)-1].Rule = " Keep  replies concise. "
	b, _ := json.Marshal(map[string]any{"notes": findings})
	if _, err := analyze(context.Background(), s, response(string(b))); err != nil {
		t.Fatal(err)
	}
	notes, err := s.list(Filter{})
	if err != nil || len(notes) != 1 || notes[0].Confirmations != 3 || notes[0].SessionCount != 1 || !notes[0].LastConfirmed.Equal(events[2].Time) {
		t.Fatalf("equivalent batch feedback split or overcounted: %+v, %v", notes, err)
	}
	if err := s.saveAnalysis(events, []Note{{EventID: events[0].ID, Rule: findings[0].Rule}}); err != nil {
		t.Fatal(err)
	}
	if got := historyNote(t, s, notes[0].ID); got.Confirmations != 3 {
		t.Fatalf("new-preference retry inflated support: %+v", got)
	}
}

func TestHistoryExplicitSupersessionIsImmediate(t *testing.T) {
	p, _, s := fixture(t)
	at := time.Now().UTC().AddDate(0, 0, -4)
	oldRule := "For Astra reviews in Project Atlas, keep explanations brief."
	old := seed(t, s, oldRule, oldRule, "communication", at)
	scopedRule := "For Opus reviews in Project Atlas, keep explanations brief."
	scoped := seed(t, s, scopedRule, scopedRule, "communication", at.Add(time.Minute))
	for i := 1; i <= 2; i++ {
		support(t, s, old, Event{Text: "Again, Astra's review explanations in Atlas should be brief.", Harness: "opencode", Model: "astra", Session: fmt.Sprintf("old-%d", i), Time: at.AddDate(0, 0, i)})
	}
	before := historyNote(t, s, old)
	feedback := []Event{
		{Text: "For Astra reviews in Atlas, I still prefer brief explanations.", Harness: "opencode", Model: "astra", Session: "old-last", Time: at.AddDate(0, 0, 2).Add(time.Hour)},
		{Text: "I've changed my mind: for Astra reviews in Project Atlas, give detailed explanations instead of brief ones.", Harness: "opencode", Model: "astra", Session: "new", Time: at.AddDate(0, 0, 3)},
		{Text: "For Opus reviews in Project Atlas, keep explanations brief as before.", Harness: "opencode", Model: "opus", Session: "other-scope", Time: at.AddDate(0, 0, 3).Add(time.Hour)},
	}
	for _, e := range feedback {
		if err := s.ingest(e); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.pending(20)
	if err != nil {
		t.Fatal(err)
	}
	// Replacement comes first in the response, before an earlier supporting event.
	findings := []Finding{
		{EventID: events[1].ID, Rule: "For Astra reviews in Project Atlas, give detailed explanations.", Category: "communication", Evidence: events[1].Text, Supersedes: []int64{old}},
		{EventID: events[0].ID, PreferenceID: old, Rule: oldRule, Category: "communication", Evidence: events[0].Text},
		{EventID: events[2].ID, PreferenceID: scoped, Rule: scopedRule, Category: "communication", Evidence: events[2].Text},
	}
	b, _ := json.Marshal(map[string]any{"notes": findings})
	if _, err := analyze(context.Background(), s, response(string(b))); err != nil {
		t.Fatal(err)
	}
	// Reopening through queryNotes checks the committed result before consolidation.
	current, err := queryNotes(context.Background(), p, Filter{Query: "Astra"})
	if err != nil || len(current) != 1 || current[0].ID == old || current[0].Rule != findings[0].Rule || current[0].Confirmations != 1 || len(current[0].History) != 1 || current[0].History[0].Evidence != events[1].Text {
		t.Fatalf("new explicit preference did not replace the repeated one immediately: %+v, %v", current, err)
	}
	previous := historyNote(t, s, old)
	if previous.Status != "superseded" || previous.Confirmations != 4 || len(previous.History) != 4 || !reflect.DeepEqual(previous.History[:3], before.History) || previous.History[3].Evidence != events[0].Text {
		t.Fatalf("supersession lost or merged conflicting history: %+v", previous)
	}
	other := historyNote(t, s, scoped)
	if other.Status != "active" || other.Confirmations != 2 || other.Rule != scopedRule || other.History[1].Evidence != events[2].Text {
		t.Fatalf("different model scope was superseded or merged: %+v", other)
	}
	active, err := s.list(Filter{})
	if err != nil || len(active) != 2 {
		t.Fatalf("unexpected active preferences after replacement: %+v, %v", active, err)
	}
}

func TestHistoryRejectsMalformedAndOutdatedSupersession(t *testing.T) {
	p, c, s := fixture(t)
	at := time.Now().UTC().AddDate(0, 0, -1)
	rule := "Keep code-review explanations brief."
	old := seed(t, s, rule, rule, "communication", at)
	removed := seed(t, s, "Always explain first.", "Explain first.", "workflow", at.Add(time.Hour))
	if err := removeNotes(p, c, s, []int64{removed}); err != nil {
		t.Fatal(err)
	}
	support(t, s, old, Event{Text: "Keep your review explanations brief.", Harness: "opencode", Session: "recent", Time: at.Add(3 * time.Hour)})
	for i, text := range []string{
		"For reviews, I want detailed explanations now.",
		"I've changed my mind: give detailed review explanations instead of brief ones.",
		"On reflection, I do want brief review explanations after all.",
	} {
		hour := []int{2, 4, 5}[i]
		if err := s.ingest(Event{Text: text, Harness: "opencode", Session: "pending", Time: at.Add(time.Duration(hour) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.pending(20)
	if err != nil {
		t.Fatal(err)
	}
	replacement := func(event int, ids ...int64) Finding {
		return Finding{EventID: events[event].ID, Rule: "Give detailed code-review explanations.", Category: "communication", Evidence: events[event].Text, Supersedes: ids}
	}
	reinforcement := func(event int) Finding {
		return Finding{EventID: events[event].ID, PreferenceID: old, Rule: rule, Category: "communication", Evidence: events[event].Text}
	}
	self := replacement(1, old)
	self.PreferenceID = old
	equivalent := replacement(1, old)
	equivalent.Rule = rule
	competing := replacement(1, old)
	competing.Rule = "Omit code-review explanations."
	before, err := s.list(Filter{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		findings []Finding
	}{
		{"unknown", []Finding{replacement(1, old, 9999)}},
		{"removed", []Finding{replacement(1, removed)}},
		{"zero", []Finding{replacement(1, 0)}},
		{"negative", []Finding{replacement(1, -1)}},
		{"duplicate target", []Finding{replacement(1, old, old)}},
		{"self", []Finding{self}},
		{"equivalent rule", []Finding{equivalent}},
		{"competing replacements", []Finding{replacement(1, old), competing}},
		{"same event supports and replaces", []Finding{replacement(1, old), reinforcement(1)}},
		{"older than persisted confirmation", []Finding{replacement(0, old)}},
		{"newer batch support after replacement", []Finding{replacement(1, old), reinforcement(2)}},
		{"newer batch support before replacement", []Finding{reinforcement(2), replacement(1, old)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{"notes": tc.findings})
			if _, err := analyze(context.Background(), s, response(string(b))); err == nil {
				t.Fatal("accepted malformed or outdated supersession")
			}
			after, err := s.list(Filter{All: true})
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("failed replacement partially changed history or statuses: %+v, %v", after, err)
			}
			pending, err := s.pending(20)
			if err != nil || !reflect.DeepEqual(pending, events) {
				t.Fatalf("failed replacement consumed pending feedback: %+v, %v", pending, err)
			}
		})
	}
}

func TestHistoryFiltersRankingAndExpiryUseSupportingFeedback(t *testing.T) {
	p, c, s := fixture(t)
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	id := seed(t, s, "Keep the patch focused.", "Limit changes to the requested scope.", "scope", day.AddDate(0, 0, -300))
	stale := seed(t, s, "Use the retired tool.", "Use the retired tool.", "tools", day.AddDate(0, 0, -299))
	newer := seed(t, s, "Answer briefly.", "Keep replies brief.", "communication", day.Add(4*time.Hour))
	support(t, s, id, Event{Text: "Across conversations, stop those unrelated rewrites.", Harness: "claude", Model: "m2", Session: "later", Time: day.AddDate(0, 0, -1).Add(3 * time.Hour)})
	support(t, s, id, Event{Text: "Stay within the requested change, even for cleanup.", Harness: "copilot", Model: "m3", Session: "later", Time: day.Add(2 * time.Hour)})
	for _, tc := range []struct {
		name string
		f    Filter
		want bool
	}{
		{"later phrase", Filter{Query: "unrelated rewrites"}, true},
		{"later model", Filter{Model: "m2"}, true},
		{"later harness and date", Filter{Harness: "copilot", Since: day, Until: day.AddDate(0, 0, 1)}, true},
		{"earlier supporting date", Filter{Since: day.AddDate(0, 0, -1), Until: day}, true},
		{"original date", Filter{Since: day.AddDate(0, 0, -300), Until: day.AddDate(0, 0, -299)}, true},
		{"mixed source observations", Filter{Model: "m2", Harness: "copilot"}, false},
		{"mixed quote and date", Filter{Query: "unrelated rewrites", Since: day}, false},
		{"mixed model and date", Filter{Model: "m2", Since: day}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.f.Category = "scope"
			notes, err := queryNotes(context.Background(), p, tc.f)
			if err != nil || (len(notes) == 1 && notes[0].ID == id) != tc.want {
				t.Fatalf("filtered feedback = %+v, %v; want match=%v", notes, err, tc.want)
			}
		})
	}
	notes, err := s.list(Filter{Limit: 1})
	if err != nil || len(notes) != 1 || notes[0].ID != id || notes[0].SessionCount != 3 {
		t.Fatalf("reinforced preference was not prominent: %+v, %v", notes, err)
	}
	var printed bytes.Buffer
	if err := printNotes(&printed, notes); err != nil || !strings.Contains(printed.String(), "confirmed 3 times") || !strings.Contains(printed.String(), "copilot · m3") || !strings.Contains(printed.String(), day.Add(2*time.Hour).Local().Format("Jan 02 15:04")) {
		t.Fatalf("CLI omitted current support: %s, %v", printed.String(), err)
	}
	g := fakeGenerator(func(prompt string, output any) error {
		var input struct {
			Preferences []Note `json:"preferences"`
		}
		if err := json.Unmarshal([]byte(prompt[strings.LastIndex(prompt, "\nData:\n")+len("\nData:\n"):]), &input); err != nil {
			return err
		}
		if len(input.Preferences) != 2 || input.Preferences[0].ID != id || len(input.Preferences[0].History) != 3 {
			t.Fatalf("consolidation ignored recent support: %+v", input)
		}
		return response(fmt.Sprintf(`{"keep_ids":[%d,%d]}`, id, newer))(prompt, output)
	})
	if _, err := consolidate(context.Background(), p, c, s, g); err != nil {
		t.Fatal(err)
	}
	if historyNote(t, s, id).Status != "active" || historyNote(t, s, stale).Status != "stale" {
		t.Fatal("expiry used original creation instead of latest confirmation")
	}
	b, err := os.ReadFile(c.AgentsFile)
	if err != nil || !strings.Contains(string(b), "(confirmed 3 times)") || strings.Index(string(b), "requested scope") > strings.Index(string(b), "replies brief") {
		t.Fatalf("publication did not prioritize reinforced feedback: %s, %v", b, err)
	}
}

func TestHistoryConsolidationPreservesDuplicateEvidence(t *testing.T) {
	p, c, s := fixture(t)
	at := time.Now().UTC().Add(-4 * time.Hour)
	id := seed(t, s, "No, focus.", "Keep edits focused.", "scope", at)
	duplicate := seed(t, s, "Do not change unrelated code when fixing a bug.", "For bug fixes, avoid unrelated changes.", "scope", at.Add(time.Hour))
	rejected := seed(t, s, "again!", "Never change anything unrelated.", "scope", at.Add(2*time.Hour))
	quotes := []string{"When fixing bugs, keep your edits within the bug's scope.", "For bug fixes, leave unrelated code alone."}
	if err := s.ingest(Event{Text: strings.Join(quotes, " "), Harness: "claude", Model: "m2", Session: "s2", Time: at.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	events, _ := s.pending(20)
	e := events[0]
	observations := []Note{}
	for i, target := range []int64{id, duplicate} {
		observations = append(observations, Note{ID: target, EventID: e.ID, Evidence: quotes[i], Harness: e.Harness, Model: e.Model, Session: e.Session, Created: e.Time})
	}
	observations = append(observations, observations[0])
	if err := s.saveAnalysis(events, observations); err != nil {
		t.Fatal(err)
	}
	original := historyNote(t, s, id)
	for _, merges := range []string{
		fmt.Sprintf(`[{"from_id":9999,"into_id":%d}]`, id),
		fmt.Sprintf(`[{"from_id":%d,"into_id":9999}]`, duplicate),
		fmt.Sprintf(`[{"from_id":%d,"into_id":%d}]`, id, id),
		fmt.Sprintf(`[{"from_id":%d,"into_id":%d},{"from_id":%d,"into_id":%d}]`, duplicate, id, duplicate, id),
	} {
		if _, err := consolidate(context.Background(), p, c, s, response(fmt.Sprintf(`{"keep_ids":[%d],"merges":%s}`, id, merges))); err == nil {
			t.Fatalf("accepted invalid merge: %s", merges)
		}
	}
	if _, err := consolidate(context.Background(), p, c, s, response(fmt.Sprintf(`{"keep_ids":[%d],"merges":[{"from_id":%d,"into_id":%d}]}`, duplicate, id, duplicate))); err == nil {
		t.Fatal("merge replaced the earlier supported identity")
	}
	rule := "When fixing bugs, keep changes focused on the bug."
	output := fmt.Sprintf(`{"keep_ids":[%d],"revisions":[{"id":%d,"rule":%q}],"merges":[{"from_id":%d,"into_id":%d}]}`, id, id, rule, duplicate, id)
	if n, err := consolidate(context.Background(), p, c, s, response(output)); err != nil || n != 1 {
		t.Fatalf("merge = %d, %v", n, err)
	}
	s.Close()
	s, err := openStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n := historyNote(t, s, id)
	if n.Rule != rule || n.EventID != original.EventID || n.Evidence != original.Evidence || !n.Created.Equal(original.Created) || n.Confirmations != 3 || n.SessionCount != 2 || len(n.History) != 4 || !n.LastConfirmed.Equal(e.Time) {
		t.Fatalf("merge lost identity/support or double-counted an event: %+v", n)
	}
	for _, o := range n.History {
		if o.Evidence == "again!" {
			t.Fatal("rejected rule became supporting evidence")
		}
	}
	for _, quote := range quotes {
		found := false
		for _, o := range n.History {
			if o.EventID == e.ID && o.Evidence == quote {
				found = true
			}
		}
		if !found {
			t.Fatalf("merge lost an exact quotation from the shared event: %q", quote)
		}
	}
	if got := historyNote(t, s, duplicate); got.Status != "superseded" || got.Evidence != "" || len(got.History) != 0 {
		t.Fatalf("restart repopulated merged source: %+v", got)
	}
	if got := historyNote(t, s, rejected); got.Status != "superseded" || got.Confirmations != 1 {
		t.Fatalf("non-merged history was changed: %+v", got)
	}
	newer := seed(t, s, "Keep replies concise.", "Keep replies concise.", "communication", at.Add(4*time.Hour))
	for i := 5; i <= 6; i++ {
		support(t, s, newer, Event{Text: "Please keep responses short.", Harness: "opencode", Session: "later", Time: at.Add(time.Duration(i) * time.Hour)})
	}
	top, err := s.list(Filter{Limit: 1})
	if err != nil || len(top) != 1 || top[0].ID != newer {
		t.Fatalf("ranking counted extra quotations as extra confirmations: %+v, %v", top, err)
	}
	if err := removeNotes(p, c, s, []int64{id}); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []int64{id, duplicate} {
		if got := historyNote(t, s, removed); got.Evidence != "" || len(got.History) != 0 {
			t.Fatalf("forgetting left merged quotations: %+v", got)
		}
	}
}
