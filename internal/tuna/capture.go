package tuna

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func capture(p Paths, h string, r io.Reader) error {
	if os.Getenv("TUNA_INTERNAL") == "1" {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r, 1024*1024))
	if err != nil {
		return err
	}
	e, err := normalizeEvent(h, b)
	if err != nil {
		return err
	}
	if e.Text == "" || strings.HasPrefix(e.Text, analysisMarker) {
		return nil
	}
	if h == "copilot" && e.Session != "" && (e.Model == "unknown" || e.Context == "") {
		model, text := transcriptContext(filepath.Join(harnessDir(p, h), "session-state", e.Session, "events.jsonl"))
		if e.Model == "unknown" && model != "" {
			e.Model = model
		}
		if e.Context == "" {
			e.Context = truncate(text, 12000)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return call(ctx, p, "POST", "/ingest", e, nil)
}

func normalizeEvent(h string, b []byte) (Event, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return Event{}, err
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	e := Event{Harness: h, Session: str("session_id", "sessionId", "session"), Model: str("model"), Text: str("prompt", "originalPrompt", "text"), Cwd: str("cwd"), Context: str("context"), Key: str("key", "message_id", "turn_id"), Time: time.Now()}
	if stamp, ok := m["timestamp"].(float64); ok {
		e.Time = time.UnixMilli(int64(stamp))
	} else if stamp := str("timestamp"); stamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			e.Time = t
		}
	}
	if e.Model == "" || e.Context == "" {
		path := str("transcript_path", "transcriptPath")
		if path != "" {
			model, text := transcriptContext(path)
			if e.Model == "" {
				e.Model = model
			}
			if e.Context == "" {
				e.Context = text
			}
		}
	}
	if e.Model == "" {
		e.Model = "unknown"
	}
	e.Context = truncate(e.Context, 12000)
	if !contains(harnesses, h) {
		return e, fmt.Errorf("unknown harness %q", h)
	}
	return e, nil
}

// Transcripts are optional context. Only the bounded tail is read; tool results are ignored.
func transcriptContext(path string) (model, text string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	if info.Size() > 256*1024 {
		if _, err = f.Seek(-256*1024, io.SeekEnd); err != nil {
			return
		}
	}
	b, _ := io.ReadAll(io.LimitReader(f, 256*1024))
	s := bufio.NewScanner(bytes.NewReader(b))
	s.Buffer(make([]byte, 4096), 256*1024)
	for s.Scan() {
		var m struct {
			Type    string `json:"type"`
			Model   string `json:"model"`
			Message struct {
				Role    string          `json:"role"`
				Model   string          `json:"model"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			Payload struct {
				Model   string          `json:"model"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"payload"`
			Data struct {
				Model   string `json:"model"`
				Content string `json:"content"`
			} `json:"data"`
		}
		if json.Unmarshal(s.Bytes(), &m) != nil {
			continue
		}
		if m.Type == "turn_context" && m.Payload.Model != "" {
			model = m.Payload.Model
		}
		if m.Type == "assistant" || m.Message.Role == "assistant" {
			if m.Message.Model != "" {
				model = m.Message.Model
			}
			if v := contentText(m.Message.Content); v != "" {
				text = v
			}
		}
		if m.Type == "response_item" && m.Payload.Role == "assistant" {
			if v := contentText(m.Payload.Content); v != "" {
				text = v
			}
		}
		if m.Type == "assistant.message" {
			if m.Data.Model != "" {
				model = m.Data.Model
			}
			if m.Data.Content != "" {
				text = m.Data.Content
			}
		}
	}
	return
}

func contentText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if p.Type == "text" || p.Type == "output_text" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}
