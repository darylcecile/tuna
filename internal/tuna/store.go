package tuna

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Event struct {
	ID      int64     `json:"id"`
	Key     string    `json:"key,omitempty"`
	Harness string    `json:"harness"`
	Session string    `json:"session"`
	Model   string    `json:"model"`
	Text    string    `json:"text"`
	Context string    `json:"context,omitempty"`
	Cwd     string    `json:"cwd,omitempty"`
	Time    time.Time `json:"time"`
}

type Note struct {
	ID            int64         `json:"id"`
	EventID       int64         `json:"event_id"`
	Rule          string        `json:"rule"`
	Category      string        `json:"category"`
	Evidence      string        `json:"evidence"`
	Harness       string        `json:"harness"`
	Model         string        `json:"model"`
	Session       string        `json:"session"`
	Created       time.Time     `json:"created"`
	Status        string        `json:"status"`
	Confirmations int           `json:"confirmations"`
	SessionCount  int           `json:"session_count"`
	LastConfirmed time.Time     `json:"last_confirmed"`
	History       []Observation `json:"history"`
	supersedes    []int64
}

type Observation struct {
	EventID  int64     `json:"event_id"`
	Evidence string    `json:"evidence"`
	Harness  string    `json:"harness"`
	Model    string    `json:"model"`
	Session  string    `json:"session"`
	Created  time.Time `json:"created"`
}

func (n *Note) summarizeHistory() {
	n.LastConfirmed = time.Time{}
	events := map[int64]bool{}
	sessions := map[string]bool{}
	for _, o := range n.History {
		events[o.EventID] = true
		if o.Created.After(n.LastConfirmed) {
			n.LastConfirmed = o.Created
		}
		if o.Session != "" {
			sessions[o.Harness+"\x00"+o.Session] = true
		}
	}
	n.Confirmations = len(events)
	n.SessionCount = len(sessions)
}

type Filter struct {
	Query    string    `json:"query,omitempty"`
	Category string    `json:"category,omitempty"`
	Model    string    `json:"model,omitempty"`
	Harness  string    `json:"harness,omitempty"`
	Since    time.Time `json:"since,omitempty"`
	Until    time.Time `json:"until,omitempty"`
	All      bool      `json:"all,omitempty"`
	Limit    int       `json:"limit,omitempty"`
}

type Store struct{ *sql.DB }

const dbTime = "2006-01-02T15:04:05.000000000Z07:00"

func openStore(p Paths) (*Store, error) {
	if err := p.init(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", p.file("tuna.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
BEGIN;
CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY, key TEXT UNIQUE NOT NULL, payload TEXT NOT NULL, processed INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS notes (id INTEGER PRIMARY KEY, event_id INTEGER NOT NULL REFERENCES events(id), rule TEXT NOT NULL, category TEXT NOT NULL, evidence TEXT NOT NULL, harness TEXT NOT NULL, model TEXT NOT NULL, session TEXT NOT NULL, created TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active');
CREATE TABLE IF NOT EXISTS state (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS observations (note_id INTEGER NOT NULL REFERENCES notes(id), event_id INTEGER NOT NULL REFERENCES events(id), evidence TEXT NOT NULL, harness TEXT NOT NULL, model TEXT NOT NULL, session TEXT NOT NULL, created TEXT NOT NULL, PRIMARY KEY(note_id,event_id,evidence));
INSERT OR IGNORE INTO observations(note_id,event_id,evidence,harness,model,session,created)
SELECT id,event_id,evidence,harness,model,session,created FROM notes
WHERE status!='removed' AND evidence!='' AND NOT EXISTS (SELECT 1 FROM state WHERE key='history_backfilled');
INSERT OR IGNORE INTO state(key,value) VALUES('history_backfilled','1');
COMMIT;`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

func (s *Store) ingest(e Event) error {
	if strings.TrimSpace(e.Text) == "" {
		return nil
	}
	if e.Harness == "" {
		return fmt.Errorf("harness is required")
	}
	if len(e.Text) > 128*1024 {
		return fmt.Errorf("prompt exceeds 128 KiB")
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Model == "" {
		e.Model = "unknown"
	}
	if e.Key == "" {
		h := sha256.Sum256([]byte(e.Harness + "\x00" + e.Session + "\x00" + e.Time.Format(time.RFC3339Nano) + "\x00" + e.Text))
		e.Key = hex.EncodeToString(h[:])
	} else {
		e.Key = e.Harness + ":" + e.Key
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = s.Exec("INSERT OR IGNORE INTO events(key,payload) VALUES(?,?)", e.Key, string(b))
	return err
}

func (s *Store) pending(limit int) ([]Event, error) {
	rows, err := s.Query("SELECT id,payload FROM events WHERE processed=0 ORDER BY id LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var id int64
		var b string
		var e Event
		if err := rows.Scan(&id, &b); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(b), &e); err != nil {
			return nil, err
		}
		e.ID = id
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) saveAnalysis(events []Event, notes []Note) error {
	tx, err := s.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	pending := map[int64]bool{}
	for _, e := range events {
		var processed bool
		if err := tx.QueryRow("SELECT processed FROM events WHERE id=?", e.ID).Scan(&processed); err != nil {
			return err
		}
		pending[e.ID] = !processed
	}
	created := map[string]int64{}
	replacements := map[int64]Note{}
	for _, n := range notes {
		if _, ok := pending[n.EventID]; !ok {
			return fmt.Errorf("preference references an event outside the batch")
		}
		if n.ID != 0 {
			var status string
			if err := tx.QueryRow("SELECT status FROM notes WHERE id=?", n.ID).Scan(&status); err != nil || status != "active" {
				return fmt.Errorf("preference target %d is not active", n.ID)
			}
		}
		if !pending[n.EventID] {
			continue
		}
		if n.ID == 0 {
			key := strings.ToLower(strings.Join(strings.Fields(n.Rule), " "))
			n.ID = created[key]
			if n.ID == 0 {
				result, err := tx.Exec(`INSERT INTO notes(event_id,rule,category,evidence,harness,model,session,created) VALUES(?,?,?,?,?,?,?,?)`, n.EventID, n.Rule, n.Category, n.Evidence, n.Harness, n.Model, n.Session, n.Created.UTC().Format(dbTime))
				if err != nil {
					return err
				}
				n.ID, err = result.LastInsertId()
				if err != nil {
					return err
				}
				created[key] = n.ID
			}
		}
		_, err = tx.Exec(`INSERT INTO observations(note_id,event_id,evidence,harness,model,session,created) VALUES(?,?,?,?,?,?,?) ON CONFLICT(note_id,event_id,evidence) DO NOTHING`, n.ID, n.EventID, n.Evidence, n.Harness, n.Model, n.Session, n.Created.UTC().Format(dbTime))
		if err != nil {
			return err
		}
		for _, id := range n.supersedes {
			previous, ok := replacements[id]
			if id == n.ID || (ok && previous.ID != n.ID) {
				return fmt.Errorf("conflicting replacement for preference %d", id)
			}
			if !ok || n.Created.After(previous.Created) || (n.Created.Equal(previous.Created) && n.EventID > previous.EventID) {
				replacements[id] = n
			}
		}
	}
	// Validate against the completed batch before changing any active statuses.
	for id, replacement := range replacements {
		var status, latest string
		var eventID int64
		if err := tx.QueryRow(`SELECT n.status,coalesce(o.created,n.created),coalesce(o.event_id,n.event_id) FROM notes n LEFT JOIN observations o ON o.note_id=n.id WHERE n.id=? ORDER BY o.created DESC,o.event_id DESC LIMIT 1`, id).Scan(&status, &latest, &eventID); err != nil || status != "active" {
			return fmt.Errorf("superseded preference target %d is not active", id)
		}
		at, err := time.Parse(time.RFC3339Nano, latest)
		if err != nil {
			return err
		}
		if replacement.Created.Before(at) || (replacement.Created.Equal(at) && replacement.EventID <= eventID) {
			return fmt.Errorf("replacement for preference %d is not newer than its supporting feedback", id)
		}
	}
	for id := range replacements {
		if _, err := tx.Exec("UPDATE notes SET status='superseded' WHERE id=?", id); err != nil {
			return err
		}
	}
	for _, e := range events {
		// Ordinary prompts need not remain in the database after classification.
		_, err = tx.Exec(`UPDATE events SET processed=1,payload='{}' WHERE id=?`, e.ID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) list(f Filter) ([]Note, error) {
	q := `SELECT n.id,n.event_id,n.rule,n.category,n.evidence,n.harness,n.model,n.session,n.created,n.status FROM notes n LEFT JOIN observations o ON o.note_id=n.id WHERE 1=1`
	args := []any{}
	if !f.All {
		q += " AND n.status='active'"
	}
	if f.Category != "" {
		q += " AND n.category=?"
		args = append(args, f.Category)
	}
	// Combined source/date filters must describe the same supporting observation.
	if f.Query != "" || f.Model != "" || f.Harness != "" || !f.Since.IsZero() || !f.Until.IsZero() {
		q += " AND EXISTS (SELECT 1 FROM notes source LEFT JOIN observations h ON h.note_id=source.id WHERE source.id=n.id"
		for _, v := range []struct{ k, v string }{{"model", f.Model}, {"harness", f.Harness}} {
			if v.v != "" {
				q += " AND coalesce(h." + v.k + ",source." + v.k + ")=?"
				args = append(args, v.v)
			}
		}
		if f.Query != "" {
			q += " AND (instr(lower(n.rule),lower(?))>0 OR instr(lower(coalesce(h.evidence,source.evidence)),lower(?))>0)"
			args = append(args, f.Query, f.Query)
		}
		if !f.Since.IsZero() {
			q += " AND coalesce(h.created,source.created)>=?"
			args = append(args, f.Since.UTC().Format(dbTime))
		}
		if !f.Until.IsZero() {
			q += " AND coalesce(h.created,source.created)<?"
			args = append(args, f.Until.UTC().Format(dbTime))
		}
		q += ")"
	}
	q += " GROUP BY n.id ORDER BY count(DISTINCT o.event_id) DESC,max(o.created) DESC,n.id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	notes := []Note{}
	byID := map[int64]int{}
	ids := []any{}
	placeholders := []string{}
	for rows.Next() {
		var n Note
		var created string
		if err := rows.Scan(&n.ID, &n.EventID, &n.Rule, &n.Category, &n.Evidence, &n.Harness, &n.Model, &n.Session, &created, &n.Status); err != nil {
			return nil, err
		}
		n.Created, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		n.History = []Observation{}
		byID[n.ID] = len(notes)
		ids = append(ids, n.ID)
		placeholders = append(placeholders, "?")
		notes = append(notes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(notes) == 0 {
		return notes, nil
	}
	history, err := s.Query(`SELECT note_id,event_id,evidence,harness,model,session,created FROM observations WHERE note_id IN (`+strings.Join(placeholders, ",")+`) ORDER BY created,event_id,evidence`, ids...)
	if err != nil {
		return nil, err
	}
	defer history.Close()
	for history.Next() {
		var id int64
		var o Observation
		var created string
		if err := history.Scan(&id, &o.EventID, &o.Evidence, &o.Harness, &o.Model, &o.Session, &created); err != nil {
			return nil, err
		}
		o.Created, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		n := &notes[byID[id]]
		n.History = append(n.History, o)
	}
	for i := range notes {
		notes[i].summarizeHistory()
	}
	return notes, history.Err()
}

func (s *Store) state(key string) string {
	var v string
	_ = s.QueryRow("SELECT value FROM state WHERE key=?", key).Scan(&v)
	return v
}

func (s *Store) setState(key, value string) error {
	_, err := s.Exec("INSERT INTO state(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}
