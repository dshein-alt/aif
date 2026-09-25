package connect

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNotesSetGetDeleteList(t *testing.T) {
	dir := t.TempDir()
	n, err := LoadNotes(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Set("b.plan", "second\nmore"); err != nil {
		t.Fatal(err)
	}
	if err := n.Set("a_1", "first"); err != nil {
		t.Fatal(err)
	}
	if got, ok := n.Get("b.plan"); !ok || got != "second\nmore" {
		t.Fatalf("get = %q %v", got, ok)
	}
	if _, ok := n.Get("nope"); ok {
		t.Fatal("missing id found")
	}
	want := []NoteHead{{"a_1", "first"}, {"b.plan", "second"}}
	if got := n.List(); !reflect.DeepEqual(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
	if err := n.Set("a_1", "replaced"); err != nil {
		t.Fatal(err)
	}
	if err := n.Delete("b.plan"); err != nil {
		t.Fatal(err)
	}
	if err := n.Delete("never-was"); err != nil {
		t.Fatalf("delete of a missing id: %v", err)
	}

	// The store survives a reload; no temp files are left behind.
	n2, err := LoadNotes(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := n2.List(); !reflect.DeepEqual(got, []NoteHead{{"a_1", "replaced"}}) {
		t.Fatalf("reloaded list = %v", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "notes.json" {
		t.Fatalf("dir holds %v", entries)
	}
}

func TestNotesLimits(t *testing.T) {
	n, _ := LoadNotes(t.TempDir())
	for _, id := range []string{"", "a b", "a/b", "ü", strings.Repeat("x", 65)} {
		if err := n.Set(id, "x"); err == nil {
			t.Fatalf("id %q accepted", id)
		}
	}
	if err := n.Set(strings.Repeat("x", 64), "x"); err != nil {
		t.Fatalf("64-char id: %v", err)
	}
	if err := n.Set("big", strings.Repeat("x", 16<<10+1)); err == nil {
		t.Fatal("note over 16 KiB accepted")
	}
	if err := n.Set("big", strings.Repeat("x", 16<<10)); err != nil {
		t.Fatalf("16 KiB note: %v", err)
	}
	for i := len(n.List()); i < 64; i++ {
		if err := n.Set(fmt.Sprint("n", i), "x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.Set("one-more", "x"); err == nil {
		t.Fatal("65th note accepted")
	}
	if err := n.Set("big", "replacing always works"); err != nil {
		t.Fatalf("replace at the limit: %v", err)
	}
}

func TestNotesBadFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.json"), []byte("{"), 0o600)
	if _, err := LoadNotes(dir); err == nil {
		t.Fatal("corrupt notes.json loaded")
	}
}
