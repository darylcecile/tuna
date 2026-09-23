package tuna

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	Analyzer      string `json:"analyzer"`
	Model         string `json:"model,omitempty"`
	AgentsFile    string `json:"agents_file"`
	ConsolidateAt string `json:"consolidate_at"`
	StaleDays     int    `json:"stale_days"`
	BatchSeconds  int    `json:"batch_seconds"`
}

type Paths struct{ Home, Dir string }

func paths(dir string) (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	if dir == "" {
		dir = os.Getenv("TUNA_HOME")
	}
	if dir == "" {
		dir = filepath.Join(home, ".local", "share", "tuna")
	}
	dir, err = filepath.Abs(dir)
	return Paths{Home: home, Dir: dir}, err
}

func (p Paths) file(name string) string { return filepath.Join(p.Dir, name) }
func (p Paths) init() error             { return os.MkdirAll(p.Dir, 0700) }

func (p Paths) config() (Config, error) {
	c := Config{Analyzer: "auto", AgentsFile: filepath.Join(p.Home, ".agents", "AGENTS.md"), ConsolidateAt: "23:00", StaleDays: 180, BatchSeconds: 30}
	b, err := os.ReadFile(p.file("config.json"))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	return c, c.validate()
}

func (c Config) validate() error {
	switch c.Analyzer {
	case "auto", "opencode", "copilot", "codex", "claude":
	default:
		return fmt.Errorf("unknown analyzer %q", c.Analyzer)
	}
	if _, err := time.Parse("15:04", c.ConsolidateAt); err != nil {
		return fmt.Errorf("consolidate_at must be HH:MM")
	}
	if c.BatchSeconds < 1 || c.StaleDays < 0 {
		return fmt.Errorf("batch_seconds must be positive and stale_days nonnegative")
	}
	if !filepath.IsAbs(c.AgentsFile) {
		return fmt.Errorf("agents_file must be an absolute path")
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'), 0600)
}

func atomicWrite(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tuna-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func envDir(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
