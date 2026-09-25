package connect

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// The local wake queue's limits.
const (
	maxWake      = 64
	maxWakeBytes = 16 << 10
)

// State is state.json, the one file the loop persists. Every transition is one Save.
type State struct {
	// The embedded Mutex is the single writer lock: the loop and every control-socket handler
	// hold it across each read-modify-write, Save included.
	sync.Mutex `json:"-"`
	dir        string

	Origin       string   `json:"origin"`
	Agent        string   `json:"agent,omitempty"`   // harness that recorded Session
	Session      string   `json:"session,omitempty"` // harness session id; empty = fresh session
	LastControl  int64    `json:"lastControl"`       // command-scan cursor; 0 = first run
	Turn         int64    `json:"turn"`
	Pending      *Pending `json:"pending,omitempty"`      // a command recorded but not yet committed
	Shutdown     *Ref     `json:"shutdown,omitempty"`     // goodbye still owed
	FreshSession *Ref     `json:"freshSession,omitempty"` // reset note still owed
	Wake         []Wake   `json:"wake,omitempty"`         // local queue, oldest first
}

// Pending is an uncommitted command: Msg for a forum command, Local (a Wake id) for a local one.
type Pending struct {
	Action string `json:"action"` // SHUTDOWN or RESET
	From   string `json:"from"`
	Msg    int64  `json:"msg,omitempty"`
	Local  string `json:"local,omitempty"`
}

// Ref names who ordered a SHUTDOWN or RESET and in which message (Msg 0 = a local command).
type Ref struct {
	From string `json:"from"`
	Msg  int64  `json:"msg,omitempty"`
}

// Wake is one queued local operator message.
type Wake struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// LoadState reads dir/state.json. A missing file is the zero state for origin; a file recorded for
// a different origin is an error (two servers must never share a state directory).
func LoadState(dir, origin string) (*State, error) {
	s := &State{dir: dir, Origin: origin}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "state.json"), err)
	}
	if s.Origin != origin {
		return nil, fmt.Errorf("%s holds origin %q, config says %q", filepath.Join(dir, "state.json"), s.Origin, origin)
	}
	return s, nil
}

// Save writes state.json atomically: temp file in the same directory, fsync, rename. The caller
// holds the lock.
// ponytail: the directory is not fsynced after the rename, so a power loss may roll back to the
// previous state (pending recovery covers it); fsync the dir on Unix if that ever matters.
func (s *State) Save() error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, "state.json.*")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(s.dir, "state.json"))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// Enqueue appends a local message to the wake queue and returns its id; it fails beyond 64
// messages or 16 KiB per message. The caller holds the lock and saves.
func (s *State) Enqueue(text string) (string, error) {
	if len(text) > maxWakeBytes {
		return "", fmt.Errorf("wake message is %d bytes, the limit is %d", len(text), maxWakeBytes)
	}
	if len(s.Wake) >= maxWake {
		return "", fmt.Errorf("wake queue is full (%d messages)", maxWake)
	}
	id := rand.Text()
	s.Wake = append(s.Wake, Wake{ID: id, Text: text})
	return id, nil
}

// Dequeue removes the given entries from the wake queue. The caller holds the lock and saves.
func (s *State) Dequeue(ids ...string) {
	s.Wake = slices.DeleteFunc(s.Wake, func(w Wake) bool { return slices.Contains(ids, w.ID) })
}

// EnsureDir creates the state directory, mode 0700.
func EnsureDir(dir string) error { return os.MkdirAll(dir, 0o700) }

// ErrLocked means another connector holds the state directory's lock.
var ErrLocked = errors.New("state directory is locked by another connector")

// Unlocker releases the state-directory lock taken by Lock.
type Unlocker interface{ Unlock() error }

type lockFile struct{ f *os.File }

// Unlock releases the lock; closing the file is enough on every OS.
func (l lockFile) Unlock() error { return l.f.Close() }

// Lock takes the exclusive, non-blocking lock on the file "lock" in dir, held until Unlock or
// process exit (the OS releases it, so a crash leaves no stale lock). A held lock is ErrLocked.
func Lock(dir string) (Unlocker, error) {
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		f.Close()
		return nil, err
	}
	return lockFile{f}, nil
}
