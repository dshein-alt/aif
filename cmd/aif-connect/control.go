package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dshein-alt/aif/internal/connect"
)

// maxStdinNote bounds `note set ID -`: the 16 KiB note limit plus the trailing newline that is
// dropped plus one byte, so a longer input still arrives over the limit and is refused.
const maxStdinNote = 16<<10 + 2

// control is a control verb (list, status, stop, wake, note) with its arguments rest, sent to the
// connector named by to; it returns the exit code.
func control(verb string, rest []string, to string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	for _, a := range rest {
		if f, _, _ := strings.Cut(a, "="); f == "--to" || f == "-to" {
			return usageError(stderr, "--to goes before the verb")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return failure(stderr, err)
	}
	var req connect.Request
	switch verb {
	case "list":
		if len(rest) != 0 || to != "" {
			return usageError(stderr, "list takes no arguments")
		}
		found := connect.List(home)
		if len(found) == 0 {
			fmt.Fprintln(stdout, "no connectors running")
		}
		for _, f := range found {
			fmt.Fprintln(stdout, f)
		}
		return 0
	case "status", "stop":
		if len(rest) != 0 {
			return usageError(stderr, "%s takes no arguments", verb)
		}
		req = connect.Request{Op: verb}
	case "wake":
		if len(rest) != 1 {
			return usageError(stderr, `wake takes one argument, the message: wake "TEXT"`)
		}
		req = connect.Request{Op: "wake", Text: rest[0]}
	case "note":
		if len(rest) == 0 {
			return usageError(stderr, "note needs an action: set|get|delete|list")
		}
		req = connect.Request{Op: "note", Action: rest[0]}
		n := len(rest) - 1
		switch rest[0] {
		case "set":
			if n != 2 {
				return usageError(stderr, "note set ID TEXT, or note set ID - to read TEXT from stdin")
			}
			req.ID, req.Note = rest[1], rest[2]
			if req.Note == "-" {
				b, err := io.ReadAll(io.LimitReader(stdin, maxStdinNote))
				if err != nil {
					return failure(stderr, err)
				}
				if req.Note = strings.TrimSuffix(string(b), "\n"); req.Note == "" {
					return failure(stderr, errors.New("note set: stdin is empty; refusing to blank the note"))
				}
			}
		case "get", "delete":
			if n != 1 {
				return usageError(stderr, "note %s ID", rest[0])
			}
			req.ID = rest[1]
		case "list":
			if n != 0 {
				return usageError(stderr, "note list takes no arguments")
			}
		default:
			return usageError(stderr, "note action must be set|get|delete|list, got %q", rest[0])
		}
	default:
		return usageError(stderr, "unknown verb %q", verb)
	}

	var dir string
	switch {
	case verb == "note" && to == "":
		if dir = getenv("AIF_CONNECT_STATE"); dir == "" {
			return failure(stderr, errors.New("not inside a connector turn; pass --to"))
		}
	default:
		if dir, err = connect.Resolve(home, to); err != nil {
			return failure(stderr, err)
		}
	}
	resp, err := connect.Call(dir, req)
	if err != nil {
		return failure(stderr, err)
	}
	if !resp.OK {
		return failure(stderr, errors.New(resp.Error))
	}
	switch {
	case verb == "status" && resp.Status != nil:
		s := resp.Status
		fmt.Fprintf(stdout, "pid: %d\nagent: %s\nharness: %s\nphase: %s\nstate: %s\nturn: %d\nreason: %s\nlastPoll: %s\nsession: %s\n",
			s.PID, s.Agent, s.Harness, s.Phase, s.State, s.Turn, s.Reason, s.LastPoll, s.Session)
		if s.VersionWarning != "" {
			fmt.Fprintf(stdout, "versionWarning: %s\n", s.VersionWarning)
		}
	case verb == "stop" || verb == "wake":
		if resp.Text == "" {
			resp.Text = "ok"
		}
		fmt.Fprintln(stdout, resp.Text)
	case req.Action == "get":
		fmt.Fprint(stdout, resp.Text)
		if !strings.HasSuffix(resp.Text, "\n") {
			fmt.Fprintln(stdout)
		}
	case req.Action == "list":
		for _, h := range resp.Notes {
			fmt.Fprintf(stdout, "%s\t%s\n", h.ID, h.First)
		}
	}
	return 0
}
