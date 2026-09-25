package connect

import (
	"reflect"
	"testing"
)

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
