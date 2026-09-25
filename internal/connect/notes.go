package connect

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// The notes store's limits.
const (
	maxNotes     = 64
	maxNoteBytes = 16 << 10
)

var noteIDRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Notes is the model's keyed text store, notes.json in the state directory. It is written only on
// the model's calls and is never part of a loop transition, so it has its own lock.
type Notes struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

// NoteHead is one line of a note listing: the id and the note's first line.
type NoteHead struct {
	ID    string `json:"id"`
	First string `json:"first"`
}

// LoadNotes reads dir/notes.json; a missing file is an empty store.
func LoadNotes(dir string) (*Notes, error) {
	n := &Notes{path: filepath.Join(dir, "notes.json"), m: map[string]string{}}
	b, err := os.ReadFile(n.path)
	if errors.Is(err, os.ErrNotExist) {
		return n, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &n.m); err != nil {
		return nil, fmt.Errorf("%s: %w", n.path, err)
	}
	return n, nil
}

// Set stores text under id, replacing any old text. A new id beyond the 64th fails.
func (n *Notes) Set(id, text string) error {
	if !noteIDRE.MatchString(id) {
		return fmt.Errorf("note id must match %s, got %q", noteIDRE, id)
	}
	if len(text) > maxNoteBytes {
		return fmt.Errorf("note is %d bytes, the limit is %d", len(text), maxNoteBytes)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	old, had := n.m[id]
	if !had && len(n.m) >= maxNotes {
		return fmt.Errorf("%d notes already; delete one first", maxNotes)
	}
	n.m[id] = text
	if err := n.save(); err != nil {
		if had {
			n.m[id] = old
		} else {
			delete(n.m, id)
		}
		return err
	}
	return nil
}

// Get returns the note stored under id.
func (n *Notes) Get(id string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	text, ok := n.m[id]
	return text, ok
}

// Delete removes the note under id; a missing id is a no-op.
func (n *Notes) Delete(id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	old, had := n.m[id]
	if !had {
		return nil
	}
	delete(n.m, id)
	if err := n.save(); err != nil {
		n.m[id] = old
		return err
	}
	return nil
}

// List returns every note's id and first line, sorted by id.
func (n *Notes) List() []NoteHead {
	n.mu.Lock()
	defer n.mu.Unlock()
	heads := []NoteHead{}
	for _, id := range slices.Sorted(maps.Keys(n.m)) {
		first, _, _ := strings.Cut(n.m[id], "\n")
		heads = append(heads, NoteHead{id, strings.TrimSuffix(first, "\r")})
	}
	return heads
}

func (n *Notes) save() error {
	b, err := json.Marshal(n.m)
	if err != nil {
		return err
	}
	return writeAtomic(n.path, b)
}

// writeAtomic writes data to path the way State.Save writes state.json: temp file in the same
// directory, fsync, rename.
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
