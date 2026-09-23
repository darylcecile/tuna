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
	ID       int64     `json:"id"`
	EventID  int64     `json:"event_id"`
	Rule     string    `json:"rule"`
	Category string    `json:"category"`
	Evidence string    `json:"evidence"`
	Harness  string    `json:"harness"`
	Model    string    `json:"model"`
	Session  string    `json:"session"`
	Created  time.Time `json:"created"`
	Status   string    `json:"status"`
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
CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY, key TEXT UNIQUE NOT NULL, payload TEXT NOT NULL, processed INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS notes (id INTEGER PRIMARY KEY, event_id INTEGER NOT NULL REFERENCES events(id), rule TEXT NOT NULL, category TEXT NOT NULL, evidence TEXT NOT NULL, harness TEXT NOT NULL, model TEXT NOT NULL, session TEXT NOT NULL, created TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active');
CREATE TABLE IF NOT EXISTS state (key TEXT PRIMARY KEY, value TEXT NOT NULL);`)
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
	for _, n := range notes {
		_, err = tx.Exec(`INSERT INTO notes(event_id,rule,category,evidence,harness,model,session,created) VALUES(?,?,?,?,?,?,?,?)`, n.EventID, n.Rule, n.Category, n.Evidence, n.Harness, n.Model, n.Session, n.Created.UTC().Format(dbTime))
		if err != nil {
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
	q := `SELECT id,event_id,rule,category,evidence,harness,model,session,created,status FROM notes WHERE 1=1`
	args := []any{}
	if !f.All {
		q += " AND status='active'"
	}
	for _, v := range []struct{ k, v string }{{"category", f.Category}, {"model", f.Model}, {"harness", f.Harness}} {
		if v.v != "" {
			q += " AND " + v.k + "=?"
			args = append(args, v.v)
		}
	}
	if f.Query != "" {
		q += " AND (instr(lower(rule),lower(?))>0 OR instr(lower(evidence),lower(?))>0)"
		args = append(args, f.Query, f.Query)
	}
	if !f.Since.IsZero() {
		q += " AND created>=?"
		args = append(args, f.Since.UTC().Format(dbTime))
	}
	if !f.Until.IsZero() {
		q += " AND created<?"
		args = append(args, f.Until.UTC().Format(dbTime))
	}
	q += " ORDER BY id DESC"
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
		notes = append(notes, n)
	}
	return notes, rows.Err()
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
