// Package driver runs an agent CLI (claude, codex, pi, opencode) as a persistent child process
// speaking that harness's own JSON-over-stdio protocol. See the "## Drivers" section of
// docs/superpowers/specs/2026-09-25-aif-connect-design.md. Every driver embeds a *proc (proc.go)
// and implements only argv building, the outgoing message format and the event mapping.
package driver

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Driver is one harness. Start may be called again after an Exit event to restart it.
type Driver interface {
	Preflight(bin string) error                     // checks the harness binary can be used (pi: its MCP adapter is installed); no-op for most
	Start(ctx context.Context, launch Launch) error // spawn, initialize, resume session if any
	Prompt(ctx context.Context, text string) error  // send one user turn
	Events() <-chan Event                           // Text, ToolCall, TurnEnd{OK|Err}, Exit
	SessionID() string                              // known after Start; the connector persists it
	Stop(ctx context.Context) error                 // graceful, then kill
	ToolHint() string                               // one sentence: how this harness names the aif MCP server's tools
	HasSystemPromptChannel() bool                   // false: the connector prepends the contract to the first prompt of a session
}

// Launch is everything a driver needs to spawn its harness.
type Launch struct {
	Bin, Cwd         string
	Model, Thinking  string
	SystemPrompt     string   // contract + role
	MCPURL, MCPToken string   // the aif MCP server and its bearer token
	Env              []string // added to (and overriding) the connector's own environment
	AutoApprove      bool
	StateDir         string       // temp files (MCP configs) go here, mode 0600
	Verbose          bool         // echo raw harness stdout lines through Log
	SessionID        string       // saved session to resume; empty starts a new one
	Log              func(string) // one line per call, no trailing newline; nil discards
}

// Kind names an Event.
type Kind string

const (
	Text     Kind = "text"
	ToolCall Kind = "tool_call"
	TurnEnd  Kind = "turn_end"
	Exit     Kind = "exit"
)

// Event is one thing the harness did. OK and Err are meaningful on TurnEnd.
type Event struct {
	Kind Kind
	Text string
	OK   bool
	Err  string
}

var (
	// ErrUnknownHarness: nothing is registered under the requested name.
	ErrUnknownHarness = errors.New("unsupported harness")
	// ErrPrerequisite: Preflight found the harness unusable (the CLI exits 1).
	ErrPrerequisite = errors.New("harness prerequisite missing")
)

var registry = map[string]func() Driver{}

// Register makes a driver available under name; drivers call it from init.
func Register(name string, ctor func() Driver) { registry[name] = ctor }

// New returns a fresh driver registered under name.
func New(name string) (Driver, error) {
	ctor, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownHarness, name)
	}
	return ctor(), nil
}

// Names lists the registered harness names, sorted.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
