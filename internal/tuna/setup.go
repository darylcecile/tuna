package tuna

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

//go:embed assets/opencode.js
var opencodePlugin string

var harnesses = []string{"opencode", "copilot", "codex", "claude"}

type SetupResult struct {
	Harness string `json:"harness"`
	Status  string `json:"status"`
}

func harnessDir(p Paths, h string) string {
	switch h {
	case "opencode":
		return filepath.Join(envDir("XDG_CONFIG_HOME", filepath.Join(p.Home, ".config")), "opencode")
	case "copilot":
		return envDir("COPILOT_HOME", filepath.Join(p.Home, ".copilot"))
	case "codex":
		return envDir("CODEX_HOME", filepath.Join(p.Home, ".codex"))
	case "claude":
		return envDir("CLAUDE_CONFIG_DIR", filepath.Join(p.Home, ".claude"))
	}
	return ""
}

func setup(p Paths, c Config, target string) ([]SetupResult, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if err := p.init(); err != nil {
		return nil, err
	}
	if target != "all" && target != "agents" && !contains(harnesses, target) {
		return nil, fmt.Errorf("choose all, agents, opencode, copilot, codex, or claude")
	}
	if err := writeJSON(p.file("config.json"), c); err != nil {
		return nil, err
	}
	if _, err := os.Stat(c.AgentsFile); os.IsNotExist(err) {
		if err := atomicWrite(c.AgentsFile, []byte("# Agent instructions\n\nUse tuna's preference tools or `tuna query --json` to look up relevant user preferences before substantial work.\n"), 0600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return nil, err
	}
	results := []SetupResult{}
	for _, h := range harnesses {
		if target != "all" && target != "agents" && target != h {
			continue
		}
		if _, err := exec.LookPath(h); err != nil && (target == "all" || target == "agents") {
			results = append(results, SetupResult{h, "not installed — skipped"})
			continue
		}
		dir := harnessDir(p, h)
		name := "AGENTS.md"
		if h == "claude" {
			name = "CLAUDE.md"
		}
		if h == "copilot" {
			name = "copilot-instructions.md"
		}
		if err := linkAgents(p, c, filepath.Join(dir, name)); err != nil {
			return results, fmt.Errorf("%s instructions: %w", h, err)
		}
		if target != "agents" {
			if err := installHarness(p, h, dir, exe); err != nil {
				return results, fmt.Errorf("%s setup: %w", h, err)
			}
		}
		status := "ready"
		if h == "codex" && target != "agents" {
			status = "installed — review and trust tuna in Codex /hooks"
		}
		results = append(results, SetupResult{h, status})
	}
	return results, nil
}

func contains(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func linkAgents(p Paths, c Config, dest string) error {
	if dest == c.AgentsFile {
		return nil
	}
	canonical, err := filepath.EvalSymlinks(c.AgentsFile)
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(dest); err == nil && resolved == canonical {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		old, err := os.ReadFile(dest)
		if err != nil {
			return err
		}
		base, err := os.ReadFile(c.AgentsFile)
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(string(old))) > 0 && !strings.Contains(string(base), strings.TrimSpace(string(old))) {
			merged := strings.TrimRight(string(base), "\n") + "\n\n" + string(old)
			if err := writeAgents(p, c, string(base), merged); err != nil {
				return err
			}
		}
		if err := os.Rename(dest, dest+".tuna-backup-"+time.Now().Format("20060102T150405.000000000")); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(canonical, dest)
}

func installHarness(p Paths, h, dir, exe string) error {
	args := []string{"--home", p.Dir, "mcp"}
	command := shellQuote(exe) + " --home " + shellQuote(p.Dir) + " capture " + h
	switch h {
	case "opencode":
		b, _ := json.Marshal(exe)
		home, _ := json.Marshal(p.Dir)
		plugin := strings.ReplaceAll(strings.ReplaceAll(opencodePlugin, "__TUNA_BINARY__", string(b)), "__TUNA_HOME__", string(home))
		return atomicWrite(filepath.Join(dir, "plugins", "tuna", "index.js"), []byte(plugin), 0600)
	case "copilot":
		hook := map[string]any{"version": 1, "hooks": map[string]any{"userPromptSubmitted": []any{map[string]any{"type": "command", "exec": exe, "args": []string{"--home", p.Dir, "capture", h}, "timeoutSec": 2}}}}
		if err := writeJSON(filepath.Join(dir, "hooks", "tuna.json"), hook); err != nil {
			return err
		}
		return editJSON(filepath.Join(dir, "mcp-config.json"), func(m map[string]any) error {
			servers, err := object(m, "mcpServers")
			if err != nil {
				return err
			}
			servers["tuna"] = map[string]any{"type": "local", "command": exe, "args": args, "tools": []string{"*"}}
			return nil
		})
	case "claude", "codex":
		file := "settings.json"
		if h == "codex" {
			file = "hooks.json"
		}
		if err := editJSON(filepath.Join(dir, file), func(m map[string]any) error {
			hooks, err := object(m, "hooks")
			if err != nil {
				return err
			}
			groups, ok := hooks["UserPromptSubmit"].([]any)
			if hooks["UserPromptSubmit"] != nil && !ok {
				return fmt.Errorf("UserPromptSubmit must be an array")
			}
			for _, g := range groups {
				gm, ok := g.(map[string]any)
				if !ok {
					continue
				}
				hs, _ := gm["hooks"].([]any)
				for _, v := range hs {
					hm, ok := v.(map[string]any)
					if !ok {
						continue
					}
					cmd, _ := hm["command"].(string)
					if cmd == command || strings.HasSuffix(cmd, " --home "+shellQuote(p.Dir)+" capture "+h) {
						hm["command"] = command
						return nil
					}
				}
			}
			hooks["UserPromptSubmit"] = append(groups, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 2}}})
			return nil
		}); err != nil {
			return err
		}
		if h == "codex" {
			return installCodexMCP(filepath.Join(dir, "config.toml"), exe, args)
		}
		mcpFile := filepath.Join(p.Home, ".claude.json")
		if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
			mcpFile = filepath.Join(dir, ".claude.json")
		}
		return editJSON(mcpFile, func(m map[string]any) error {
			servers, err := object(m, "mcpServers")
			if err != nil {
				return err
			}
			servers["tuna"] = map[string]any{"type": "stdio", "command": exe, "args": args}
			return nil
		})
	}
	return nil
}

func object(m map[string]any, key string) (map[string]any, error) {
	if m[key] == nil {
		m[key] = map[string]any{}
	}
	v, ok := m[key].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", key)
	}
	return v, nil
}

func editJSON(path string, edit func(map[string]any) error) error {
	m := map[string]any{}
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if m == nil {
		return fmt.Errorf("%s must contain an object", path)
	}
	if err := edit(m); err != nil {
		return err
	}
	updated, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	updated = append(updated, '\n')
	if string(b) == string(updated) {
		return nil
	}
	if len(b) > 0 {
		if err := backupConfig(path, b); err != nil {
			return err
		}
	}
	return atomicWrite(path, updated, 0600)
}

func backupConfig(path string, b []byte) error {
	return atomicWrite(path+".tuna-backup-"+time.Now().Format("20060102T150405.000000000"), b, 0600)
}

func installCodexMCP(path, exe string, args []string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(b)
	const start = "# tuna:mcp:start"
	const end = "# tuna:mcp:end"
	i, j := strings.Index(text, start), strings.Index(text, end)
	if i >= 0 && j > i {
		text = text[:i] + text[j+len(end):]
	} else if i >= 0 || j >= 0 {
		return fmt.Errorf("malformed tuna markers in %s", path)
	}
	var cfg map[string]any
	if err := toml.Unmarshal([]byte(text), &cfg); err != nil {
		return err
	}
	if servers, ok := cfg["mcp_servers"].(map[string]any); ok && servers["tuna"] != nil {
		return fmt.Errorf("%s already defines mcp_servers.tuna outside tuna's managed block", path)
	}
	block, err := toml.Marshal(map[string]any{"command": exe, "args": args})
	if err != nil {
		return err
	}
	updated := strings.TrimRight(text, "\n") + "\n\n" + start + "\n[mcp_servers.tuna]\n" + string(block) + end + "\n"
	if updated == string(b) {
		return nil
	}
	if len(b) > 0 {
		if err := backupConfig(path, b); err != nil {
			return err
		}
	}
	return atomicWrite(path, []byte(updated), 0600)
}
