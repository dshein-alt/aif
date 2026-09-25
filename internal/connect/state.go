package connect

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

// LoadState reads dir/state.json; the caller holds the directory lock (LockDir). A missing file is
// the zero state for origin with fresh true, the signal for the first-run rule. An existing file
// without an origin is corrupt, and one recorded for a different origin is an error (two servers
// must never share a state directory); both errors name the file. Temp files a crashed Save left
// behind are removed.
func LoadState(dir, origin string) (s *State, fresh bool, err error) {
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "state.json.") {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	p := filepath.Join(dir, "state.json")
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &State{dir: dir, Origin: origin}, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	s = &State{dir: dir}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, false, fmt.Errorf("%s: %w", p, err)
	}
	switch s.Origin {
	case "":
		return nil, false, fmt.Errorf("%s: no origin (corrupt state file)", p)
	case origin:
		return s, false, nil
	}
	return nil, false, fmt.Errorf("%s holds origin %q, config says %q", p, s.Origin, origin)
}

// Save writes state.json atomically (writeAtomic). The caller holds s's mutex.
func (s *State) Save() error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.dir, "state.json"), b)
}

// writeAtomic replaces path with data: temp file path.* in the same directory, fsync, rename; the
// temp file is removed on failure. State.Save and the notes store both write through it.
// ponytail: the directory is not fsynced after the rename, so a power loss may roll back to the
// previous file (pending recovery covers state.json); fsync the dir on Unix if that ever matters.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
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
		return "", fmt.Errorf("wake queue is full (%d); use stop or wait for a turn", maxWake)
	}
	id := rand.Text()
	s.Wake = append(s.Wake, Wake{ID: id, Text: text})
	return id, nil
}

// Dequeue removes the given entries from the wake queue. The caller holds the lock and saves.
func (s *State) Dequeue(ids ...string) {
	s.Wake = slices.DeleteFunc(s.Wake, func(w Wake) bool { return slices.Contains(ids, w.ID) })
}

// EnsureDir creates the state directory, mode 0700, and tightens an existing one to 0700 (it
// holds control.sock) on Unix.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil || runtime.GOOS == "windows" {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// ErrLocked means another connector holds the state directory's lock.
var ErrLocked = errors.New("state directory is locked by another connector")

// LockDir takes the exclusive, non-blocking lock on the file "lock" in dir and returns the func
// that releases it. A held lock is ErrLocked. Keep the returned func reachable and call it at exit
// (an unreachable lock file may be closed by the GC, which drops the lock); the OS releases the
// lock if the process dies, so a crash leaves no stale lock.
func LockDir(dir string) (unlock func() error, err error) {
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		f.Close()
		return nil, err
	}
	return func() error { return unlockFile(f) }, nil
}
