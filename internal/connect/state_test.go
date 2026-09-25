package connect

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testOrigin = "https://example.com:443"

func TestStateMissingIsZero(t *testing.T) {
	s, err := LoadState(t.TempDir(), testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if s.Origin != testOrigin || s.Session != "" || s.LastControl != 0 || s.Turn != 0 ||
		s.Pending != nil || s.Shutdown != nil || s.FreshSession != nil || len(s.Wake) != 0 {
		t.Fatalf("not the zero state: %+v", s)
	}
}

func TestStateRoundTripAndOrigin(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadState(dir, testOrigin)
	s.Agent, s.Session, s.LastControl, s.Turn = "claude", "sess", 4711, 12
	s.Pending = &Pending{Action: "RESET", From: "TheRoot", Msg: 4712}
	s.FreshSession = &Ref{From: "TheRoot", Msg: 4712}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if strings.Contains(string(b), "shutdown") || strings.Contains(string(b), "wake") || strings.Contains(string(b), "local") {
		t.Fatalf("unset fields are written: %s", b)
	}
	got, err := LoadState(dir, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if got.Session != "sess" || got.LastControl != 4711 || got.Turn != 12 || *got.Pending != *s.Pending || *got.FreshSession != *s.FreshSession {
		t.Fatalf("round trip: %+v", got)
	}
	if _, err := LoadState(dir, "http://example.com:80"); err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("origin mismatch: %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("temp files left behind: %v", ents)
	}
}

func TestStateSaveAtomic(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadState(dir, testOrigin)
	big := strings.Repeat("x", 16<<10)
	for range maxWake {
		if _, err := s.Enqueue(big); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() {
		for i := range 50 {
			s.Lock()
			s.Turn = int64(i)
			err := s.Save()
			s.Unlock()
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		default:
		}
		b, err := os.ReadFile(filepath.Join(dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		var got State
		if err := json.Unmarshal(b, &got); err != nil || len(got.Wake) != maxWake {
			t.Fatalf("partial read (%d bytes): %v", len(b), err)
		}
	}
}

func TestWakeQueue(t *testing.T) {
	s, _ := LoadState(t.TempDir(), testOrigin)
	if _, err := s.Enqueue(strings.Repeat("x", 16<<10+1)); err == nil {
		t.Fatal("a message over 16 KiB was queued")
	}
	var ids []string
	for i := range maxWake {
		id, err := s.Enqueue(string(rune('a' + i%26)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := s.Enqueue("one too many"); err == nil {
		t.Fatal("a 65th message was queued")
	}
	s.Dequeue(ids[0], ids[2])
	if len(s.Wake) != maxWake-2 || s.Wake[0].ID != ids[1] || s.Wake[1].ID != ids[3] {
		t.Fatalf("dequeue kept the wrong entries: %v", s.Wake[:2])
	}
	if _, err := s.Enqueue("fits again"); err != nil {
		t.Fatal(err)
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	u, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Lock: %v, want ErrLocked", err)
	}
	if err := u.Unlock(); err != nil {
		t.Fatal(err)
	}
	u, err = Lock(dir)
	if err != nil {
		t.Fatalf("Lock after Unlock: %v", err)
	}
	u.Unlock()
}

func TestEnsureDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".aif-connect", "mybot@https_example.com_443")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("not created: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %04o, want 0700", fi.Mode().Perm())
	}
}
