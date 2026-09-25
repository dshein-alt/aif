package connect

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	callTimeout = 10 * time.Second // one control exchange, both sides
	maxRequest  = 1 << 20          // a 16 KiB wake or note, JSON-escaped, fits with room to spare
)

// Request is one control-socket request, a single JSON line. Op is status, stop, wake (Text is the
// message) or note (Action is set|get|delete|list, ID and Note its arguments).
type Request struct {
	Op     string `json:"op"`
	Text   string `json:"text,omitempty"`
	Action string `json:"action,omitempty"`
	ID     string `json:"id,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Response is the one JSON reply: {"ok":true,...} or {"ok":false,"error":"..."}. Text carries a
// note's text (or whatever the handler says), Notes a note listing, Status the status reply.
type Response struct {
	OK     bool       `json:"ok"`
	Error  string     `json:"error,omitempty"`
	Text   string     `json:"text,omitempty"`
	Notes  []NoteHead `json:"notes,omitempty"`
	Status *Status    `json:"status,omitempty"`
}

// Status is the connector's answer to op status.
type Status struct {
	PID            int    `json:"pid"`
	Agent          string `json:"agent"`   // the AIF agent name
	Harness        string `json:"harness"` // claude|codex|pi|opencode
	Phase          string `json:"phase"`   // starting|running
	Turn           int64  `json:"turn"`
	Reason         string `json:"reason"`
	State          string `json:"state"`    // idle|turn|stopping
	LastPoll       string `json:"lastPoll"` // RFC3339, "" before the first poll
	Session        string `json:"session"`
	VersionWarning string `json:"versionWarning"`
}

// Handler answers one request; the loop is the handler. It is called concurrently, one call per
// connection.
type Handler func(Request) Response

// Server is a bound control socket.
type Server struct {
	ln net.Listener
	wg sync.WaitGroup
}

// Serve binds dir/control.sock, removing a stale one first (the caller holds the state-directory
// lock, so nobody else can own it), and serves each connection's request: op note from notes
// when notes is non-nil, everything else through h.
// ponytail: a Unix socket path is limited to ~108 bytes; a very long home or agent name fails
// here, bind relative to the directory if that ever bites.
func Serve(dir string, notes *Notes, h Handler) (*Server, error) {
	sock := filepath.Join(dir, "control.sock")
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln}
	s.wg.Go(func() {
		for {
			c, err := ln.Accept()
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if err != nil { // out of descriptors or similar: back off, keep serving
				time.Sleep(100 * time.Millisecond)
				continue
			}
			s.wg.Go(func() { serveConn(c, notes, h) })
		}
	})
	return s, nil
}

func serveConn(c net.Conn, notes *Notes, h Handler) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(callTimeout))
	var req Request
	var resp Response
	switch err := json.NewDecoder(io.LimitReader(c, maxRequest)).Decode(&req); {
	case err != nil:
		resp = Response{Error: "bad request: " + err.Error()}
	case req.Op == "note" && notes != nil:
		resp = notes.handle(req)
	default:
		resp = h(req)
	}
	json.NewEncoder(c).Encode(resp)
}

// Close stops accepting, waits for the requests in flight and removes the socket.
func (s *Server) Close() {
	s.ln.Close() // net removes the socket file it created
	s.wg.Wait()
}

func (n *Notes) handle(r Request) Response {
	var err error
	switch r.Action {
	case "set":
		err = n.Set(r.ID, r.Note)
	case "get":
		text, ok := n.Get(r.ID)
		if !ok {
			return Response{Error: fmt.Sprintf("no note %q", r.ID)}
		}
		return Response{OK: true, Text: text}
	case "delete":
		err = n.Delete(r.ID)
	case "list":
		return Response{OK: true, Notes: n.List()}
	default:
		return Response{Error: fmt.Sprintf("note action must be set|get|delete|list, got %q", r.Action)}
	}
	if err != nil {
		return Response{Error: err.Error()}
	}
	return Response{OK: true}
}

// Call sends one request to the connector whose state directory is dir and returns its reply.
func Call(dir string, req Request) (Response, error) {
	c, err := net.DialTimeout("unix", filepath.Join(dir, "control.sock"), callTimeout)
	if err != nil {
		return Response{}, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(callTimeout))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("%s: %w", filepath.Join(dir, "control.sock"), err)
	}
	return resp, nil
}

// Found is one running connector: its state directory, the directory's name (name@origin-key)
// and its status reply.
type Found struct {
	Dir    string
	Name   string
	Status Status
}

// String is the connector's line in `aif-connect list`: PID  name@server  harness  state  turn.
func (f Found) String() string {
	return fmt.Sprintf("%d  %s  %s  %s  %d", f.Status.PID, f.Name, f.Status.Harness, f.Status.State, f.Status.Turn)
}

// List finds the running connectors under home/.aif-connect, sorted by directory name. A socket
// that does not answer status (a stale one left by a crash) is skipped.
func List(home string) []Found {
	socks, _ := filepath.Glob(filepath.Join(home, ".aif-connect", "*", "control.sock"))
	var found []Found
	for _, sock := range socks {
		dir := filepath.Dir(sock)
		if r, err := Call(dir, Request{Op: "status"}); err == nil && r.OK && r.Status != nil {
			found = append(found, Found{dir, filepath.Base(dir), *r.Status})
		}
	}
	return found
}

// Resolve picks the running connector named by to: a PID, a bare agent name (unique among the
// running name@… directories; case-insensitive, as AIF names are) or name@origin-key. An empty to
// resolves when exactly one connector is running. The errors list the running ones.
func Resolve(home, to string) (string, error) {
	running := List(home)
	if len(running) == 0 {
		return "", errors.New("no aif-connect connector is running")
	}
	var hits []Found
	for _, f := range running {
		name, _, _ := strings.Cut(f.Name, "@")
		if to == "" || to == strconv.Itoa(f.Status.PID) || strings.EqualFold(to, f.Name) || strings.EqualFold(to, name) {
			hits = append(hits, f)
		}
	}
	if len(hits) == 1 {
		return hits[0].Dir, nil
	}
	var lines []string
	for _, f := range running {
		lines = append(lines, f.String())
	}
	switch {
	case to == "":
		return "", fmt.Errorf("several connectors are running; pick one with --to:\n%s", strings.Join(lines, "\n"))
	case len(hits) == 0:
		return "", fmt.Errorf("no running connector matches %q; running:\n%s", to, strings.Join(lines, "\n"))
	default:
		return "", fmt.Errorf("%q matches several running connectors; use name@server or the PID:\n%s", to, strings.Join(lines, "\n"))
	}
}
