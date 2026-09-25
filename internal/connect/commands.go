package connect

import (
	"regexp"
	"slices"
	"strings"
)

// cmdRe is the one command marker: #CMD[NAME args]#. NAME is case-sensitive: an upper-case
// letter, then upper-case letters, digits or underscores; args run lazily to the first "]#".
var cmdRe = regexp.MustCompile(`#CMD\[([A-Z][A-Z0-9_]*)(?:\s+(.*?))?\]#`)

// Command is one #CMD[...]# marker found in a message body.
type Command struct {
	Name string
	Args string
}

// Hit is the command a scan decided to execute.
type Hit struct {
	ID     int64
	From   string
	Action string // "SHUTDOWN" or "RESET"
}

// Extract returns every command marker in body, in order.
func Extract(body string) []Command {
	var out []Command
	for _, m := range cmdRe.FindAllStringSubmatch(body, -1) {
		out = append(out, Command{Name: m[1], Args: m[2]})
	}
	return out
}

// Scan picks the command to execute from a feed batch: only operators' messages that sit in the
// home thread or tag agentName count. SHUTDOWN beats RESET wherever they are in the batch; among
// several of the winning action the latest (highest id) supplies From. Hit.ID is the highest id
// of any known command that counted, whatever its action, so a cursor advanced to it consumes the
// batch whole. isOperator must match names case-insensitively (Config.IsOperator does).
// Unknown names are returned for logging and otherwise ignored.
func Scan(msgs []Message, thread int64, agentName string, isOperator func(string) bool) (hit *Hit, unknown []string) {
	rank := map[string]int{"RESET": 1, "SHUTDOWN": 2}
	var last int64
	for _, m := range msgs {
		if !isOperator(m.Author) {
			continue
		}
		if m.Thread != thread && !slices.ContainsFunc(m.At, func(a string) bool { return strings.EqualFold(a, agentName) }) {
			continue
		}
		for _, c := range Extract(m.Body) {
			r, known := rank[c.Name]
			if !known {
				unknown = append(unknown, c.Name)
				continue
			}
			last = max(last, m.ID)
			if hit == nil || r > rank[hit.Action] || (r == rank[hit.Action] && m.ID > hit.ID) {
				hit = &Hit{ID: m.ID, From: m.Author, Action: c.Name}
			}
		}
	}
	if hit != nil {
		hit.ID = last
	}
	return hit, unknown
}
