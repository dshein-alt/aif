package connect

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func newScanState(t *testing.T, st *State) *State {
	t.Helper()
	st.dir, st.Origin = t.TempDir(), "http://x:80"
	return st
}

func reload(t *testing.T, st *State) *State {
	t.Helper()
	s, _, err := LoadState(st.dir, st.Origin)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func feedOf(ms []Message, seq int64, err error) func(context.Context, int64) ([]Message, int64, error) {
	return func(context.Context, int64) ([]Message, int64, error) { return ms, seq, err }
}

func TestRunScanBookkeeping(t *testing.T) {
	cfg := Config{Thread: 3, AgentName: "mybot", Operators: []string{"root"}}
	ctx := context.Background()

	// clean: the cursor moves to the reply's seq, even past messages the scan does not count
	st := newScanState(t, &State{LastControl: 5})
	p, _, err := RunScan(ctx, feedOf([]Message{{ID: 6, Thread: 3, Author: "x", Body: "#CMD[RESET]#"}}, 20, nil), st, cfg)
	if p != nil || err != nil || reload(t, st).LastControl != 20 {
		t.Errorf("clean scan: %+v %v", p, err)
	}
	// a failed feed changes nothing
	p, _, err = RunScan(ctx, feedOf(nil, 0, ErrUnreachable), st, cfg)
	if p != nil || !errors.Is(err, ErrUnreachable) || st.LastControl != 20 {
		t.Errorf("failed scan: %+v %v %d", p, err, st.LastControl)
	}

	// a hit is the first write: pending recorded, the cursor untouched until the commit
	st = newScanState(t, &State{LastControl: 5, Session: "s1"})
	p, unknown, err := RunScan(ctx, feedOf([]Message{
		{ID: 8, Thread: 3, Author: "root", Body: "#CMD[RESET]#"},
		{ID: 9, Thread: 3, Author: "root", Body: "#CMD[NOPE]#"},
		{ID: 10, Thread: 3, Author: "x", Body: "hi"},
	}, 10, nil), st, cfg)
	want := &Pending{Action: "RESET", From: "root", Msg: 8}
	if err != nil || !reflect.DeepEqual(p, want) || !reflect.DeepEqual(unknown, []string{"NOPE"}) {
		t.Fatalf("hit: %+v %v %v", p, unknown, err)
	}
	if got := reload(t, st); !reflect.DeepEqual(got.Pending, want) || got.LastControl != 5 || got.Session != "s1" {
		t.Errorf("after the first write: %+v", got)
	}
	if err := Commit(st); err != nil {
		t.Fatal(err)
	}
	got := reload(t, st)
	if got.Pending != nil || got.LastControl != 8 || got.Session != "" || !reflect.DeepEqual(got.FreshSession, &Ref{From: "root", Msg: 8}) {
		t.Errorf("after the commit: %+v", got)
	}
}

func TestLocalCommand(t *testing.T) {
	st := newScanState(t, &State{LastControl: 7, Wake: []Wake{{ID: "a", Text: "hi"}, {ID: "b", Text: "#CMD[SHUTDOWN]# "}, {ID: "c", Text: "#CMD[SHUTDOWN]#"}}})
	if got := promptWake(st.Wake); len(got) != 2 || got[1].ID != "b" {
		t.Errorf("promptWake %+v: only an exact marker is a command", got)
	}
	p, err := LocalScan(st)
	if err != nil || !reflect.DeepEqual(p, &Pending{Action: "SHUTDOWN", From: "local", Local: "c"}) {
		t.Fatalf("LocalScan %+v %v", p, err)
	}
	if err := Commit(st); err != nil {
		t.Fatal(err)
	}
	got := reload(t, st)
	if got.Pending != nil || got.LastControl != 7 || len(got.Wake) != 2 || !reflect.DeepEqual(got.Shutdown, &Ref{From: "local"}) {
		t.Errorf("after the commit: %+v", got)
	}
	if p, _ := LocalScan(st); p != nil {
		t.Errorf("second LocalScan %+v", p)
	}
}

func TestExtract(t *testing.T) {
	cases := map[string][]Command{
		"@mybot #CMD[RESET]#":       {{Name: "RESET"}},
		"#CMD[SHUTDOWN]#.":          {{Name: "SHUTDOWN"}},
		"#CMD[FOO bar baz]#":        {{Name: "FOO", Args: "bar baz"}},
		"a #CMD[RESET]# b #CMD[X]#": {{Name: "RESET"}, {Name: "X"}},
		"reset":                     nil,
		"please RESET the password": nil,
		"#cmd[reset]#":              nil,
		"#CMD[Reset]#":              nil,
		"#CMD[RESET]":               nil,
		"#CMD[RESET2]#":             {{Name: "RESET2"}},
		"#CMD[A_1 x]#":              {{Name: "A_1", Args: "x"}},
		"#CMD[2X]#":                 nil,
		"#CMD[FOO a]b]#":            {{Name: "FOO", Args: "a]b"}},
		"#CMD[FOO a]# #CMD[BAR]#":   {{Name: "FOO", Args: "a"}, {Name: "BAR"}},
	}
	for body, want := range cases {
		if got := Extract(body); !reflect.DeepEqual(got, want) {
			t.Errorf("Extract(%q) = %+v, want %+v", body, got, want)
		}
	}
}

func TestScan(t *testing.T) {
	ops := func(name string) bool { return name == "David" || name == "Root" }
	const home, me = 3, "mybot"

	cases := []struct {
		name    string
		msgs    []Message
		hit     *Hit
		unknown []string
	}{
		{"nothing", []Message{{ID: 1, Thread: home, Author: "David", Body: "hello RESET"}}, nil, nil},
		{"home thread", []Message{{ID: 4, Thread: home, Author: "David", Body: "#CMD[RESET]#"}},
			&Hit{ID: 4, From: "David", Action: "RESET"}, nil},
		{"tagged elsewhere, case-insensitive", []Message{{ID: 5, Thread: 1, Author: "Root", Body: "@MyBot #CMD[SHUTDOWN]#", At: []string{"MyBot"}}},
			&Hit{ID: 5, From: "Root", Action: "SHUTDOWN"}, nil},
		{"not an operator", []Message{{ID: 6, Thread: home, Author: "mallory", Body: "#CMD[SHUTDOWN]#"}}, nil, nil},
		{"operator in another thread, untagged", []Message{{ID: 7, Thread: 1, Author: "David", Body: "#CMD[RESET]#", At: []string{"other"}}}, nil, nil},
		{"home thread but tagging another resident: theirs, not mine", []Message{{ID: 7, Thread: home, Author: "David", Body: "@other #CMD[SHUTDOWN]#", At: []string{"other"}}}, nil, nil},
		{"home thread, tags me among others: mine", []Message{{ID: 7, Thread: home, Author: "David", Body: "#CMD[RESET]#", At: []string{"other", "mybot"}}}, &Hit{ID: 7, From: "David", Action: "RESET"}, nil},
		{"shutdown before a later reset: the reset is consumed too", []Message{
			{ID: 8, Thread: home, Author: "David", Body: "#CMD[SHUTDOWN]#"},
			{ID: 9, Thread: home, Author: "Root", Body: "#CMD[RESET]#"},
		}, &Hit{ID: 9, From: "David", Action: "SHUTDOWN"}, nil},
		{"shutdown beats earlier reset", []Message{
			{ID: 8, Thread: home, Author: "David", Body: "#CMD[RESET]#"},
			{ID: 9, Thread: home, Author: "Root", Body: "#CMD[SHUTDOWN]#"},
		}, &Hit{ID: 9, From: "Root", Action: "SHUTDOWN"}, nil},
		{"latest of the winning action", []Message{
			{ID: 8, Thread: home, Author: "David", Body: "#CMD[SHUTDOWN]#"},
			{ID: 9, Thread: home, Author: "Root", Body: "#CMD[SHUTDOWN]#"},
		}, &Hit{ID: 9, From: "Root", Action: "SHUTDOWN"}, nil},
		{"unknown ignored", []Message{
			{ID: 10, Thread: home, Author: "David", Body: "#CMD[PAUSE 5m]#"},
			{ID: 11, Thread: home, Author: "David", Body: "#CMD[RESET]#"},
		}, &Hit{ID: 11, From: "David", Action: "RESET"}, []string{"PAUSE"}},
		{"unknown does not move the id", []Message{
			{ID: 12, Thread: home, Author: "David", Body: "#CMD[RESET]#"},
			{ID: 13, Thread: home, Author: "David", Body: "#CMD[RESET2]#"},
		}, &Hit{ID: 12, From: "David", Action: "RESET"}, []string{"RESET2"}},
	}
	for _, c := range cases {
		hit, unknown := Scan(c.msgs, home, me, ops)
		if !reflect.DeepEqual(hit, c.hit) || !reflect.DeepEqual(unknown, c.unknown) {
			t.Errorf("%s: Scan = %+v, %v; want %+v, %v", c.name, hit, unknown, c.hit, c.unknown)
		}
	}
}
