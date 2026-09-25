package connect

import (
	"regexp"
	"slices"
	"strings"
)

// cmdRe is the one command marker: #CMD[NAME args]#, NAME upper-case and case-sensitive.
var cmdRe = regexp.MustCompile(`#CMD\[([A-Z]+)(?:\s+([^\]]*))?\]#`)

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
// several of the winning action the latest (highest id) wins, so the cursor passes them all.
// Unknown names are returned for logging and otherwise ignored.
func Scan(msgs []Message, thread int64, agentName string, isOperator func(string) bool) (hit *Hit, unknown []string) {
	rank := map[string]int{"RESET": 1, "SHUTDOWN": 2}
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
			if hit == nil || r > rank[hit.Action] || (r == rank[hit.Action] && m.ID > hit.ID) {
				hit = &Hit{ID: m.ID, From: m.Author, Action: c.Name}
			}
		}
	}
	return hit, unknown
}
