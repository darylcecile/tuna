package tuna

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLiveRuleQuality(t *testing.T) {
	analyzer := os.Getenv("TUNA_LIVE_ANALYZER")
	if analyzer == "" {
		t.Skip("set TUNA_LIVE_ANALYZER to an installed, authenticated harness to evaluate rule quality")
	}
	switch analyzer {
	case "opencode", "copilot", "claude", "codex":
	default:
		t.Fatalf("unsupported TUNA_LIVE_ANALYZER %q", analyzer)
	}

	t.Run("extraction", func(t *testing.T) {
		p, c, s := liveQualityFixture(t, analyzer)
		cases := []struct {
			name, text, context string
			properties          []string
			scope, quotation    string
		}{
			{
				name: "temporary push hold", text: "Hold this push while I look over the diff.",
				context: "The patch is ready to push.",
			},
			{
				name: "software complaint", text: "The release job keeps hanging until I hit rerun in the dashboard.",
				context: "We are debugging the deployment service.",
			},
			{
				name: "agent product requirement", text: "The agent should have access to a GitHub sandbox for repo tasks, including opening PRs.",
				context: "We are designing capabilities for the in-app assistant in our product.",
			},
			{
				name: "local feature request", text: "Make the confirmation button green and put it below the form.",
			},
			{
				name: "bare assent", text: "Yes, go ahead.",
				context: "Shall I rebase this branch and push the fix? I could make that my default workflow.",
			},
			{
				name: "polite correction", text: "Could you keep your explanations short and start with the answer, please?",
				properties: []string{`short|brief|concis`, `answer|conclusion`, `first|start|lead|begin`},
			},
			{
				name: "implicit durable feedback", text: "You're spending more effort on test scaffolding than the fix. Test the behavior you changed, not unrelated parts of the project.",
				properties: []string{`test`, `chang|affect|modif`, `only|limit|focus|restrict|unrelated`},
			},
			{
				name: "working method during a fix", text: "Refactor loading/state synchronization naturally, not deferred setter tricks or lint suppressions.",
				context:    "We are fixing lint failures in the assistant's React components.",
				properties: []string{`refactor|natural`, `lint|suppress`, `defer|setter`},
			},
			{
				name: "intent correction during a task", text: "preserve original inventory intent (do not add exclusion absent original)",
				properties: []string{`intent|meaning|scope`, `exclu|constraint|restrict|filter`},
			},
			{
				name: "conditional pushing", text: "When I ask you to fix review comments on a PR, that includes pushing the resulting changes.",
				properties: []string{`\bpush`, `review|comment|feedback`},
				scope:      `(?i)\b(when|if|for|on|after|upon)\b.*\b(pr|pull request)s?\b`, quotation: "When I ask you to fix review comments on a PR",
			},
			{
				name: "explicit model scope", text: "When Opus reviews code, I want bugs ahead of stylistic suggestions.",
				properties: []string{`bug|defect|issue|finding`, `before|ahead|first|priorit|lead`},
				scope:      `(?i)opus.*review|review.*opus`, quotation: "When Opus reviews code",
			},
		}
		for i, tc := range cases {
			if err := s.ingest(Event{Key: fmt.Sprintf("quality-%d", i), Harness: "opencode", Model: "github-copilot/gpt-6-astra", Session: "session-1", Text: tc.text, Context: tc.context, Time: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
		events, err := s.pending(20)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := analyze(context.Background(), s, HarnessGenerator{p, c}); err != nil {
			t.Fatal(err)
		}
		notes, err := s.list(Filter{All: true})
		if err != nil {
			t.Fatal(err)
		}
		byEvent := map[int64][]Note{}
		for _, n := range notes {
			t.Logf("event %d: %s; evidence=%q", n.EventID, n.Rule, n.Evidence)
			byEvent[n.EventID] = append(byEvent[n.EventID], n)
		}
		for i, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				found := byEvent[events[i].ID]
				if len(tc.properties) == 0 {
					if len(found) != 0 {
						t.Errorf("learned a preference from a one-off or unsupported request: %+v", found)
					}
					return
				}
				if len(found) == 0 {
					t.Fatal("missed reusable feedback")
				}
				var rules []string
				for _, n := range found {
					rules = append(rules, n.Rule)
					if regexp.MustCompile(`(?i)astra|gpt|copilot|opencode`).MatchString(n.Rule) {
						t.Errorf("turned attribution into rule scope: %q", n.Rule)
					}
					if tc.scope != "" && !regexp.MustCompile(tc.scope).MatchString(n.Rule) {
						t.Errorf("lost condition/model scope: %q", n.Rule)
					}
					if !strings.Contains(n.Evidence, tc.quotation) {
						t.Errorf("evidence lost the qualifying condition: %q", n.Evidence)
					}
				}
				text := strings.ToLower(strings.Join(rules, "\n"))
				for _, property := range tc.properties {
					if !regexp.MustCompile(property).MatchString(text) {
						t.Errorf("missing semantic property %q in %q", property, text)
					}
				}
			})
		}
	})

	t.Run("consolidation", func(t *testing.T) {
		p, c, s := liveQualityFixture(t, analyzer)
		cases := []struct {
			evidence, rule, category string
			keep                     bool
		}{
			{"Hold this push while I look over the diff.", "Do not push changes until the user has reviewed them.", "workflow", false},
			{"The build stalls until I press rerun in the dashboard.", "Automatically retry stalled builds.", "workflow", false},
			{"the agent should have access to a github sandbox for Repo-related tasks. For example if im asking it to raise a PR, it should use the sandbox to do that.", "For repository-related tasks, use a GitHub sandbox.", "tools", false},
			{"Yes, go ahead.", "Proceed autonomously whenever implementation is needed.", "workflow", false},
			{"there is a comment", "Before completing PR work, address all review comments.", "workflow", false},
			{"When I ask you to fix review comments on a PR, that includes pushing the resulting changes.", "When asked to fix PR review comments, push the resulting changes.", "workflow", true},
			{"For release branches, ask me before pushing any commits.", "Ask before pushing commits to release branches.", "workflow", true},
			{"No, stop writing so many tests. Only test changed behavior.", "Test only changed behavior.", "testing", true},
			{"Refactor loading/state synchronization naturally, not deferred setter tricks or lint suppressions.", "Fix lint violations through natural refactoring; do not weaken rules, add suppressions, or use deferred state-setter tricks.", "implementation", true},
			{"preserve original inventory intent (do not add exclusion absent original)", "Preserve the original request's intent; do not invent constraints or exclusions that are absent from it.", "scope", true},
			{"Please keep replies concise; the long introductions are too much.", "Keep replies concise.", "communication", true},
		}
		ids := make([]int64, len(cases))
		for i, tc := range cases {
			ids[i] = seed(t, s, tc.evidence, tc.rule, tc.category, time.Now())
		}
		scoped := seed(t, s, "When Opus reviews code, I want bugs ahead of stylistic suggestions.", "Lead all code reviews with bugs rather than style suggestions.", "communication", time.Now())
		duplicate := seed(t, s, "When Opus reviews code, I want bugs ahead of stylistic suggestions.", "When Opus reviews code, lead with bugs rather than stylistic suggestions.", "communication", time.Now())
		revisionID := seed(t, s, "When editing database migrations, explain the rollback plan first.", "Before editing any code, explain the rollback plan.", "workflow", time.Now())
		base := "# My rules\n\nKeep commits focused.\nDo not automatically retry stalled builds.\nFor repository-related tasks, use a GitHub sandbox.\nKeep replies concise.\n"
		if err := os.WriteFile(c.AgentsFile, []byte(base), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := consolidate(context.Background(), p, c, s, HarnessGenerator{p, c}); err != nil {
			t.Fatal(err)
		}
		notes, err := s.list(Filter{All: true})
		if err != nil {
			t.Fatal(err)
		}
		statuses := map[int64]string{}
		var revised Note
		var merged Note
		for _, n := range notes {
			t.Logf("note %d [%s]: %s; evidence=%q", n.ID, n.Status, n.Rule, n.Evidence)
			statuses[n.ID] = n.Status
			if n.ID == revisionID {
				revised = n
			}
			if n.ID == scoped {
				merged = n
			}
		}
		if revised.Status != "active" || !regexp.MustCompile(`(?i)(when|for|before|on).*migration`).MatchString(revised.Rule) || !strings.Contains(strings.ToLower(revised.Rule), "rollback") {
			t.Errorf("did not restore the supported condition: %+v", revised)
		}
		if merged.Status != "active" || !regexp.MustCompile(`(?i)opus.*review|review.*opus`).MatchString(merged.Rule) || merged.Confirmations != 2 || statuses[duplicate] != "superseded" {
			t.Errorf("did not restore model scope and preserve duplicate evidence under the earlier ID: %+v; duplicate status=%s", merged, statuses[duplicate])
		}
		published, err := os.ReadFile(c.AgentsFile)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("published instructions:\n%s", published)
		if !strings.Contains(string(published), fmt.Sprintf("- %s <!-- tuna:%d -->", revised.Rule, revisionID)) {
			t.Error("revised preference was not published with its original ID")
		}
		if !strings.Contains(string(published), fmt.Sprintf("<!-- tuna:%d -->", scoped)) || strings.Contains(string(published), fmt.Sprintf("<!-- tuna:%d -->", duplicate)) {
			t.Error("published duplicates did not use the stable preference ID")
		}
		for i, tc := range cases {
			want := "superseded"
			if tc.keep {
				want = "active"
			}
			if statuses[ids[i]] != want {
				t.Errorf("%q: status=%q, want %q", tc.rule, statuses[ids[i]], want)
			}
			if strings.Contains(string(published), fmt.Sprintf("<!-- tuna:%d -->", ids[i])) != tc.keep {
				t.Errorf("%q: published membership does not match keep=%v", tc.rule, tc.keep)
			}
		}
		handwritten, err := splitAgents(string(published))
		if err != nil {
			t.Fatal(err)
		}
		wantBase := strings.Replace(base, "Keep replies concise.\n", "", 1)
		if strings.TrimSpace(handwritten) != strings.TrimSpace(wantBase) {
			t.Errorf("instruction edits went beyond the supported duplicate removal:\n%s", handwritten)
		}
		if strings.Contains(handwritten, "Keep replies concise.") || strings.Count(string(published), "Keep replies concise.") != 1 {
			t.Error("supported duplicate was not moved into the managed preferences")
		}
	})
}

func TestLiveFeedbackHistory(t *testing.T) {
	analyzer := os.Getenv("TUNA_LIVE_ANALYZER")
	if analyzer == "" {
		t.Skip("set TUNA_LIVE_ANALYZER to evaluate cross-session feedback")
	}
	p, c, s := liveQualityFixture(t, analyzer)
	now := time.Now().UTC()
	implementation := seed(t, s, "When I ask you to implement something, finish the agreed work instead of stopping after analysis.", "When implementation is requested, carry the agreed work through to completion.", "workflow", now.AddDate(0, 0, -14))
	brief := seed(t, s, "For code reviews, keep your explanations brief.", "Keep code-review explanations brief.", "communication", now.AddDate(0, 0, -10))
	release := seed(t, s, "For release branches, ask before pushing.", "Ask before pushing to release branches.", "workflow", now.AddDate(0, 0, -9))
	for i, quote := range []string{"I prefer short code-review findings.", "Keep code-review findings concise, please."} {
		e := Event{Key: fmt.Sprintf("earlier-%d", i), Harness: "opencode", Model: "provider/model", Session: fmt.Sprintf("earlier-review-%d", i), Text: quote, Time: now.AddDate(0, 0, -8+i)}
		if err := s.ingest(e); err != nil {
			t.Fatal(err)
		}
		events, err := s.pending(20)
		if err != nil || len(events) != 1 {
			t.Fatalf("seed prior confirmation: %+v, %v", events, err)
		}
		if err := s.saveAnalysis(events, []Note{{ID: brief, EventID: events[0].ID, Rule: "Keep code-review explanations brief.", Category: "communication", Evidence: quote, Harness: e.Harness, Model: e.Model, Session: e.Session, Created: e.Time}}); err != nil {
			t.Fatal(err)
		}
	}
	feedback := []Event{
		{Key: "retrospective", Harness: "opencode", Model: "provider/astra", Session: "work-monday", Time: now.AddDate(0, 0, -2), Text: "I've told you ten times in other sessions: when I ask for an implementation, finish it instead of just explaining what you would do."},
		{Key: "rhetorical", Harness: "copilot", Model: "other-model", Session: "work-tuesday", Time: now.AddDate(0, 0, -1), Text: "Again, you treated my 'are you going to actually do it?' as something to answer. In implementation tasks, carry on with the agreed work instead of stopping there."},
		{Key: "vague", Harness: "opencode", Model: "provider/astra", Session: "work-wednesday", Time: now, Text: "This is ridiculous.", Context: "I described the implementation approach."},
		{Key: "changed-mind", Harness: "opencode", Model: "provider/astra", Session: "review-wednesday", Time: now, Text: "I've changed my mind about code reviews: give me detailed reasoning for each finding rather than brief summaries."},
		{Key: "different-scope", Harness: "opencode", Model: "provider/astra", Session: "branch-wednesday", Time: now, Text: "For feature branches, push once the changes pass their tests."},
		{Key: "quoted-example", Harness: "opencode", Model: "provider/astra", Session: "docs-wednesday", Time: now, Text: "For the help page, show this sample complaint: 'I've told you to finish implementation, not just describe it.'"},
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
	g := HarnessGenerator{p, c}
	if _, err := analyze(context.Background(), s, g); err != nil {
		t.Fatal(err)
	}
	notes, err := s.list(Filter{All: true})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]Note{}
	var detailed, feature int64
	for _, n := range notes {
		t.Logf("after analysis: id=%d confirmations=%d sessions=%d last=%s rule=%q history=%+v", n.ID, n.Confirmations, n.SessionCount, n.LastConfirmed, n.Rule, n.History)
		byID[n.ID] = n
		if n.EventID == events[3].ID {
			detailed = n.ID
		}
		if n.EventID == events[4].ID {
			feature = n.ID
		}
	}
	if len(notes) != 5 || detailed == 0 || feature == 0 {
		t.Fatalf("expected three existing preferences and two new, differently scoped/contrary preferences: %+v", notes)
	}
	reinforced := byID[implementation]
	if reinforced.Confirmations != 3 || reinforced.SessionCount != 3 || len(reinforced.History) != 3 || !reinforced.LastConfirmed.Equal(feedback[1].Time) {
		t.Fatalf("cross-session corrections did not reinforce the stable preference exactly twice: %+v", reinforced)
	}
	for _, e := range events[:2] {
		found := false
		for _, h := range reinforced.History {
			if h.EventID == e.ID {
				found = h.Session == e.Session && h.Harness == e.Harness && h.Model == e.Model && h.Created.Equal(e.Time) && h.Evidence != "" && strings.Contains(e.Text, h.Evidence)
			}
		}
		if !found {
			t.Errorf("lost supporting quote or attribution for event %d", e.ID)
		}
	}
	if byID[brief].Confirmations != 3 || byID[release].Confirmations != 1 || byID[detailed].Confirmations != 1 || byID[feature].Confirmations != 1 {
		t.Fatalf("changed intent or branch scope was counted as reinforcement: %+v", notes)
	}
	if byID[brief].Status != "superseded" || byID[detailed].Status != "active" {
		t.Errorf("newer explicit preference did not take effect during analysis: old=%+v new=%+v", byID[brief], byID[detailed])
	}
	current, err := queryNotes(context.Background(), p, Filter{Query: "review", Limit: 1})
	if err != nil || len(current) != 1 || current[0].ID != detailed {
		t.Errorf("lookup preferred the repeated old preference before consolidation: %+v, %v", current, err)
	}
	if notes[0].ID != implementation {
		t.Errorf("reinforced, recently confirmed preference was not prominent in retrieval: %+v", notes)
	}
	if t.Failed() {
		return
	}
	if _, err := consolidate(context.Background(), p, c, s, g); err != nil {
		t.Fatal(err)
	}
	notes, err = s.list(Filter{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		t.Logf("after consolidation: id=%d status=%s confirmations=%d rule=%q", n.ID, n.Status, n.Confirmations, n.Rule)
		byID[n.ID] = n
	}
	if byID[implementation].Status != "active" || byID[implementation].Confirmations != 3 {
		t.Errorf("consolidation lost reinforced preference/history: %+v", byID[implementation])
	}
	if byID[brief].Status != "superseded" || byID[detailed].Status != "active" {
		t.Errorf("repetition outweighed the newer explicit change of mind: old=%+v new=%+v", byID[brief], byID[detailed])
	}
	if byID[release].Status != "active" || byID[feature].Status != "active" {
		t.Errorf("different branch conditions were treated as conflicting: release=%+v feature=%+v", byID[release], byID[feature])
	}
	published, err := os.ReadFile(c.AgentsFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("published instructions:\n%s", published)
	priority := strings.Index(string(published), fmt.Sprintf("<!-- tuna:%d -->", implementation))
	ordinary := strings.Index(string(published), fmt.Sprintf("<!-- tuna:%d -->", release))
	if priority < 0 || ordinary < 0 || priority >= ordinary {
		t.Errorf("reinforcement did not increase published prominence:\n%s", published)
	}
}

func liveQualityFixture(t *testing.T, analyzer string) (Paths, Config, *Store) {
	t.Helper()
	// Keep Tuna's data temporary while using the harness's normal authentication.
	keys := []string{"XDG_CONFIG_HOME", "COPILOT_HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR"}
	env := map[string]string{}
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	p, c, s := fixture(t)
	for _, key := range keys {
		if value, ok := env[key]; ok {
			t.Setenv(key, value)
		} else if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	c.Analyzer, c.Model = analyzer, os.Getenv("TUNA_LIVE_MODEL")
	t.Logf("live analyzer=%s model=%q (empty uses the harness default)", c.Analyzer, c.Model)
	return p, c, s
}
