package tuna

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func Execute(version string) error { return newCommand(version).Execute() }

func newCommand(version string) *cobra.Command {
	var home string
	var asJSON bool
	root := &cobra.Command{Use: "tuna", Short: "An ear for feedback. A memory for better work.", Version: version, SilenceUsage: true, SilenceErrors: true, Long: "Tuna learns how you like agents to work, across your coding harnesses.\nStart once, work normally, and let your preferences travel with you."}
	root.PersistentFlags().StringVar(&home, "home", "", "data directory (default: $TUNA_HOME or ~/.local/share/tuna)")
	root.PersistentFlags().BoolVar(&asJSON, "json", false, "machine-readable output")
	get := func() (Paths, Config, error) {
		p, err := paths(home)
		if err != nil {
			return p, Config{}, err
		}
		c, err := p.config()
		return p, c, err
	}
	output := func(cmd *cobra.Command, v any, text string) error {
		if asJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(v)
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout(), text)
		return err
	}
	add := func(use, short string, args cobra.PositionalArgs, fn func(*cobra.Command, []string) error) *cobra.Command {
		cmd := &cobra.Command{Use: use, Short: short, Args: args, RunE: fn}
		root.AddCommand(cmd)
		return cmd
	}
	add("start", "Set up installed harnesses and start listening", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		p, c, err := get()
		if err != nil {
			return err
		}
		if running(p) {
			return output(cmd, map[string]bool{"running": true}, "Tuna is already listening.")
		}
		if _, err := selectAnalyzer(c.Analyzer); err != nil {
			return err
		}
		result, err := setup(p, c, "all")
		if err != nil {
			return err
		}
		if err := start(p); err != nil {
			return err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Tuna is listening.\n\n%s\n\nPreferences → %s\nDaily consolidation → %s (local time)\nUse tuna status or tuna list today to check in.", formatSetup(result), c.AgentsFile, c.ConsolidateAt)
		return output(cmd, map[string]any{"running": true, "integrations": result, "agents_file": c.AgentsFile}, b.String())
	})
	add("stop", "Stop listening; keep your preferences", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		p, _, err := get()
		if err != nil {
			return err
		}
		if err := stop(p); err != nil {
			return err
		}
		return output(cmd, map[string]bool{"running": false}, "Tuna is resting. Your preferences are saved.")
	})
	add("restart", "Restart the background service", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		p, c, err := get()
		if err != nil {
			return err
		}
		if err := stop(p); err != nil {
			return err
		}
		if _, err := setup(p, c, "all"); err != nil {
			return err
		}
		if err := start(p); err != nil {
			return err
		}
		return output(cmd, map[string]bool{"running": true}, "Tuna is listening again.")
	})
	add("status", "Show service health and learning progress", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		p, _, err := get()
		if err != nil {
			return err
		}
		st := Status{Home: p.Dir}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Second)
		defer cancel()
		if err := call(ctx, p, "GET", "/status", nil, &st); err != nil {
			return output(cmd, st, "Tuna is resting. Run tuna start to begin listening.")
		}
		text := fmt.Sprintf("Tuna is listening · %s\n\n  Preferences   %d\n  Pending       %d\n  Analyzer      %s\n  Consolidated  %s\n  Data          %s", st.Version, st.Notes, st.Pending, st.Analyzer, or(st.LastConsolidated, "not yet"), p.Dir)
		if st.LastError != "" {
			text += "\n\nNeeds attention: " + st.LastError
		}
		return output(cmd, st, text)
	})
	var analyzer, model, agentsFile, at string
	var stale int
	setupCmd := add("setup [all|agents|opencode|copilot|codex|claude]", "Install integrations or configure learning", cobra.MaximumNArgs(1), func(cmd *cobra.Command, args []string) error {
		p, c, err := get()
		if err != nil {
			return err
		}
		if analyzer != "" {
			c.Analyzer = analyzer
		}
		if cmd.Flags().Changed("model") {
			c.Model = model
		}
		if agentsFile != "" {
			c.AgentsFile, err = filepath.Abs(agentsFile)
			if err != nil {
				return err
			}
		}
		if at != "" {
			c.ConsolidateAt = at
		}
		if cmd.Flags().Changed("stale-days") {
			c.StaleDays = stale
		}
		if err := c.validate(); err != nil {
			return err
		}
		wasRunning := running(p)
		if wasRunning {
			if err := stop(p); err != nil {
				return err
			}
		}
		target := "all"
		if len(args) > 0 {
			target = args[0]
		}
		result, err := setup(p, c, target)
		if err != nil {
			return err
		}
		if wasRunning {
			if err := start(p); err != nil {
				return err
			}
		}
		return output(cmd, map[string]any{"integrations": result, "config": c}, "Tuna is set up.\n\n"+formatSetup(result)+"\n\nInstructions → "+c.AgentsFile+"\nConfig       → "+p.file("config.json"))
	})
	setupCmd.Flags().StringVar(&analyzer, "analyzer", "", "auto, opencode, copilot, codex, or claude")
	setupCmd.Flags().StringVar(&model, "model", "", "analysis model (uses the harness default if empty)")
	setupCmd.Flags().StringVar(&agentsFile, "agents-file", "", "canonical global AGENTS.md path")
	setupCmd.Flags().StringVar(&at, "at", "", "daily consolidation time, HH:MM in local time")
	setupCmd.Flags().IntVar(&stale, "stale-days", 180, "expire unrefreshed preferences after this many days; 0 disables expiry")
	for _, kind := range []string{"list", "query"} {
		f := Filter{Limit: 100}
		use, short := "list [today|yesterday|week ago|YYYY-MM-DD]", "Browse learned preferences by date"
		if kind == "query" {
			use = "query [text]"
			short = "Find preferences relevant to your work"
		}
		cmd := add(use, short, cobra.ArbitraryArgs, func(cmd *cobra.Command, args []string) error {
			p, _, err := get()
			if err != nil {
				return err
			}
			if kind == "query" {
				f.Query = strings.Join(args, " ")
			} else {
				f.Since, f.Until, err = dateRange(strings.Join(args, " "), time.Now())
				if err != nil {
					return err
				}
			}
			if f.Limit < 1 {
				return fmt.Errorf("limit must be positive")
			}
			notes, err := queryNotes(cmd.Context(), p, f)
			if err != nil {
				return err
			}
			if asJSON {
				return output(cmd, notes, "")
			}
			if len(notes) == 0 {
				return output(cmd, notes, "No matching preferences yet. Tuna learns from corrections as you work.")
			}
			return printNotes(cmd.OutOrStdout(), notes)
		})
		cmd.Flags().StringVar(&f.Category, "category", "", "filter by category")
		cmd.Flags().StringVar(&f.Model, "model", "", "filter by exact source model")
		cmd.Flags().StringVar(&f.Harness, "harness", "", "filter by source harness")
		cmd.Flags().BoolVar(&f.All, "all", false, "include removed, stale, and superseded notes")
		cmd.Flags().IntVar(&f.Limit, "limit", 100, "maximum results")
	}
	add("remove <id> [id...]", "Forget preferences and remove their published rules", cobra.MinimumNArgs(1), func(cmd *cobra.Command, args []string) error {
		p, _, err := get()
		if err != nil {
			return err
		}
		ids := []int64{}
		for _, arg := range args {
			n, err := strconv.ParseInt(arg, 10, 64)
			if err != nil || n < 1 {
				return fmt.Errorf("%q is not a preference ID", arg)
			}
			ids = append(ids, n)
		}
		var result any
		if err := call(cmd.Context(), p, "POST", "/remove", map[string]any{"ids": ids}, &result); err != nil {
			return err
		}
		return output(cmd, result, fmt.Sprintf("Forgot %d preference(s). Published rules have been updated.", len(ids)))
	})
	add("consolidate", "Process pending feedback and refresh AGENTS.md now", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		p, _, err := get()
		if err != nil {
			return err
		}
		var result struct {
			Preferences int `json:"preferences"`
		}
		if err := call(cmd.Context(), p, "POST", "/consolidate", nil, &result); err != nil {
			return err
		}
		return output(cmd, result, fmt.Sprintf("AGENTS.md refreshed with %d preference(s).", result.Preferences))
	})
	add("mcp", "Serve preference tools over MCP stdio", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		p, _, err := get()
		if err != nil {
			return err
		}
		return runMCP(cmd.Context(), p, version)
	})
	cap := add("capture <harness>", "Receive a harness prompt on stdin", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		// Hook failures must not surface in or block the user's coding session.
		p, err := paths(home)
		if err == nil {
			_ = capture(p, args[0], cmd.InOrStdin())
		}
		return nil
	})
	cap.Hidden = true
	daemon := add("serve", "Run the service in the foreground", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		p, _, err := get()
		if err != nil {
			return err
		}
		return serve(p, version)
	})
	daemon.Hidden = true
	var from string
	updateCmd := add("update", "Install the latest release or a local binary", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		p, _, err := get()
		if err != nil {
			return err
		}
		v, err := update(cmd.Context(), p, from, version)
		if err != nil {
			return err
		}
		return output(cmd, map[string]string{"version": v}, "Tuna updated to "+v+".")
	})
	updateCmd.Flags().StringVar(&from, "from", "", "install a locally built tuna binary")
	root.SetHelpCommand(&cobra.Command{Use: "help [command]", Short: "Help about any command", RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := root.Find(args)
		if err != nil {
			return err
		}
		return c.Help()
	}})
	return root
}

func queryNotes(ctx context.Context, p Paths, f Filter) ([]Note, error) {
	if running(p) {
		var notes []Note
		err := call(ctx, p, "POST", "/query", f, &notes)
		return notes, err
	}
	if _, err := os.Stat(p.file("tuna.db")); os.IsNotExist(err) {
		return []Note{}, nil
	}
	s, err := openStore(p)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.list(f)
}

func formatSetup(results []SetupResult) string {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "  %-10s %s\n", r.Harness, r.Status)
	}
	return strings.TrimRight(b.String(), "\n")
}

func printNotes(w io.Writer, notes []Note) error {
	t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(t, "ID\tWHEN\tCATEGORY\tSOURCE\tPREFERENCE")
	for _, n := range notes {
		rule := n.Rule
		if n.Status != "active" {
			rule = "[" + n.Status + "] " + rule
		}
		fmt.Fprintf(t, "%d\t%s\t%s\t%s · %s\t%s\n", n.ID, n.Created.Local().Format("Jan 02 15:04"), n.Category, n.Harness, n.Model, rule)
	}
	return t.Flush()
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func dateRange(s string, now time.Time) (time.Time, time.Time, error) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "all":
		return time.Time{}, time.Time{}, nil
	case "today":
		return day, day.AddDate(0, 0, 1), nil
	case "yesterday":
		return day.AddDate(0, 0, -1), day, nil
	case "week ago", "a week ago", "last week":
		return day.AddDate(0, 0, -7), now, nil
	}
	date, err := time.ParseInLocation("2006-01-02", s, now.Location())
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("unknown date %q; use today, yesterday, 'week ago', or YYYY-MM-DD", s)
	}
	return date, date.AddDate(0, 0, 1), nil
}
