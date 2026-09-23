package tuna

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type Service struct {
	p       Paths
	c       Config
	s       *Store
	g       Generator
	mu      sync.Mutex
	cancel  context.CancelFunc
	ctx     context.Context
	version string
}

type Status struct {
	Running          bool   `json:"running"`
	Version          string `json:"version,omitempty"`
	Analyzer         string `json:"analyzer,omitempty"`
	Pending          int    `json:"pending"`
	Notes            int    `json:"notes"`
	LastConsolidated string `json:"last_consolidated,omitempty"`
	LastError        string `json:"last_error,omitempty"`
	Home             string `json:"home"`
}

func serve(p Paths, version string) error {
	if err := p.init(); err != nil {
		return err
	}
	lock, err := lockFile(p.file("service.lock"))
	if err != nil {
		return fmt.Errorf("service already running or locked: %w", err)
	}
	defer lock.Close()
	c, err := p.config()
	if err != nil {
		return err
	}
	s, err := openStore(p)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	svc := &Service{p: p, c: c, s: s, g: HarnessGenerator{p, c}, cancel: cancel, ctx: ctx, version: version}
	_ = os.Remove(p.file("tuna.sock"))
	l, err := net.Listen("unix", p.file("tuna.sock"))
	if err != nil {
		return err
	}
	defer l.Close()
	defer os.Remove(p.file("tuna.sock"))
	if err := os.Chmod(p.file("tuna.sock"), 0600); err != nil {
		return err
	}
	server := &http.Server{Handler: svc.handler(), ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() { defer close(done); svc.work() }()
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = server.Shutdown(shutdown)
	}()
	err = server.Serve(l)
	cancel()
	<-done
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Service) status() Status {
	st := Status{Running: true, Version: s.version, Analyzer: s.c.Analyzer, Home: s.p.Dir, LastConsolidated: s.s.state("consolidated"), LastError: s.s.state("error")}
	_ = s.s.QueryRow("SELECT count(*) FROM events WHERE processed=0").Scan(&st.Pending)
	_ = s.s.QueryRow("SELECT count(*) FROM notes WHERE status='active'").Scan(&st.Notes)
	return st
}

func (s *Service) recordError(err error) {
	if err != nil {
		log.Print(err)
		_ = s.s.setState("error", err.Error())
	} else {
		_ = s.s.setState("error", "")
	}
}

func (s *Service) work() {
	ticker := time.NewTicker(time.Duration(s.c.BatchSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		s.mu.Lock()
		_, err := analyze(s.ctx, s.s, s.g)
		if err == nil && consolidationDue(time.Now(), s.c.ConsolidateAt, s.s.state("consolidated")) {
			var pending int
			if err = s.s.QueryRow("SELECT count(*) FROM events WHERE processed=0").Scan(&pending); err == nil && pending == 0 {
				_, err = consolidate(s.ctx, s.p, s.c, s.s, s.g)
			}
		}
		s.recordError(err)
		s.mu.Unlock()
	}
}

func consolidationDue(now time.Time, at, last string) bool {
	if last == now.Format("2006-01-02") {
		return false
	}
	return now.Format("15:04") >= at
}

func (s *Service) handler() http.Handler {
	mux := http.NewServeMux()
	wrap := func(pattern string, fn func(http.ResponseWriter, *http.Request) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			v, err := fn(w, r)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				v = map[string]string{"error": err.Error()}
			}
			_ = json.NewEncoder(w).Encode(v)
		})
	}
	wrap("GET /status", func(w http.ResponseWriter, r *http.Request) (any, error) { return s.status(), nil })
	wrap("POST /stop", func(w http.ResponseWriter, r *http.Request) (any, error) {
		s.cancel()
		return map[string]bool{"stopped": true}, nil
	})
	wrap("POST /ingest", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var e Event
		if err := decodeRequest(w, r, &e); err != nil {
			return nil, err
		}
		return map[string]bool{"accepted": true}, s.s.ingest(e)
	})
	wrap("POST /query", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var f Filter
		if err := decodeRequest(w, r, &f); err != nil {
			return nil, err
		}
		return s.s.list(f)
	})
	wrap("POST /remove", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var body struct {
			IDs []int64 `json:"ids"`
		}
		if err := decodeRequest(w, r, &body); err != nil {
			return nil, err
		}
		if len(body.IDs) == 0 {
			return nil, fmt.Errorf("specify at least one note ID")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return map[string]any{"removed": body.IDs}, removeNotes(s.p, s.c, s.s, body.IDs)
	})
	wrap("POST /consolidate", func(w http.ResponseWriter, r *http.Request) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		for {
			pending, err := s.s.pending(1)
			if err != nil {
				return nil, err
			}
			if len(pending) == 0 {
				break
			}
			if _, err = analyze(s.ctx, s.s, s.g); err != nil {
				s.recordError(err)
				return nil, err
			}
		}
		n, err := consolidate(s.ctx, s.p, s.c, s.s, s.g)
		s.recordError(err)
		return map[string]int{"preferences": n}, err
	})
	return mux
}

func decodeRequest(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024)).Decode(v)
}

func call(ctx context.Context, p Paths, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", p.file("tuna.sock"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, method, "http://tuna"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach tuna; run 'tuna start': %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s", e.Error)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func running(p Paths) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return call(ctx, p, "GET", "/status", nil, &Status{}) == nil
}

func start(p Paths) error {
	if running(p) {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p.file("service.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(exe, "--home", p.Dir, "serve")
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Dir = p.Dir
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if running(p) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("service did not start; see %s", p.file("service.log"))
}

func stop(p Paths) error {
	if !running(p) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := call(ctx, p, "POST", "/stop", nil, nil); err != nil {
		return err
	}
	for i := 0; i < 100; i++ {
		lock, err := lockFile(p.file("service.lock"))
		if err == nil {
			lock.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("service is still stopping; check %s", p.file("service.log"))
}
