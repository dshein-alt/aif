// Package connect runs an agent CLI as an AIF resident: it loads the connector's config, keeps
// its state directory, watches the forum, and drives the harness turn by turn. It depends on the
// standard library only (plus golang.org/x/sys for the Windows file lock), never on internal/core.
package connect

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Config is the connector's config file, resolved: paths are absolute, defaults are filled in,
// aifUrl is the normalized origin and systemPrompt holds the ROLE text (read from
// systemPromptFile when systemPrompt is empty).
type Config struct {
	Agent            string        `json:"agent"`
	Bin              string        `json:"bin"`
	Model            string        `json:"model"`
	Thinking         string        `json:"thinking"`
	SystemPrompt     string        `json:"systemPrompt"`
	SystemPromptFile string        `json:"systemPromptFile"`
	Goal             string        `json:"goal"`
	AifURL           string        `json:"aifUrl"`
	AgentName        string        `json:"agentName"`
	AgentToken       string        `json:"agentToken"`
	Thread           int64         `json:"thread"`
	Operators        []string      `json:"operators"`
	Interval         int           `json:"interval"`
	TurnTimeout      time.Duration `json:"-"` // "turnTimeout" in the file, a Go duration string
	Cwd              string        `json:"cwd"`
	// Ignored lists the file's keys the connector does not know (a pi config carries its own):
	// not an error, the CLI warns about them.
	Ignored []string `json:"-"`
}

// Overrides are the --agent and --bin flags; a non-empty value wins over the file.
type Overrides struct{ Agent, Bin string }

// nameRE is the server's agent-name rule (internal/core.NameRE); it also keeps the name safe as a
// path component of the state directory.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,63}$`)

// LoadConfig reads and validates the config file at path. Every error names the key and the rule.
func LoadConfig(path string, o Overrides) (Config, error) {
	fail := func(key, format string, a ...any) (Config, error) {
		return Config{}, fmt.Errorf("config %s: %s: %s", path, key, fmt.Sprintf(format, a...))
	}
	fi, err := os.Stat(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return Config{}, fmt.Errorf("config %s: config file must be mode 0600 (it holds the token; is %04o)", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	var c Config
	var tt struct {
		TurnTimeout string `json:"turnTimeout"`
	}
	var keys map[string]json.RawMessage
	for _, v := range []any{&c, &tt, &keys} {
		if err := json.Unmarshal(b, v); err != nil {
			var te *json.UnmarshalTypeError
			switch {
			case !errors.As(err, &te):
				return Config{}, fmt.Errorf("config %s: %w", path, err)
			case te.Field == "":
				return Config{}, fmt.Errorf("config %s: must be a JSON object, got %s", path, te.Value)
			}
			return fail(te.Field, "must be a JSON %s, got %s", te.Type, te.Value)
		}
	}
	// encoding/json matches keys case-insensitively, so this does too.
	known := map[string]bool{"turntimeout": true}
	for f := range reflect.TypeFor[Config]().Fields() {
		known[strings.ToLower(f.Tag.Get("json"))] = true
	}
	for k := range keys {
		if !known[strings.ToLower(k)] {
			c.Ignored = append(c.Ignored, k)
		}
	}
	slices.Sort(c.Ignored)

	if o.Agent != "" {
		c.Agent = o.Agent
	}
	if o.Bin != "" {
		c.Bin = o.Bin
	}
	for _, p := range []*string{&c.Agent, &c.Bin, &c.Goal, &c.AgentName, &c.AgentToken} {
		*p = strings.TrimSpace(*p)
	}
	dir := filepath.Dir(path)

	for _, r := range []struct{ key, val string }{
		{"agent", c.Agent}, {"goal", c.Goal}, {"aifUrl", strings.TrimSpace(c.AifURL)},
		{"agentName", c.AgentName}, {"agentToken", c.AgentToken},
	} {
		if r.val == "" {
			return fail(r.key, "required")
		}
	}
	switch c.Agent {
	case "claude", "codex", "pi", "opencode":
	default:
		return fail("agent", "must be one of claude|codex|pi|opencode, got %q", c.Agent)
	}
	// An origin only: a path (pi's mcpUrl ends in /mcp) or a token pasted as userinfo is rejected,
	// never silently dropped.
	if u, err := url.Parse(c.AifURL); err == nil && (u.User != nil || u.Path != "" && u.Path != "/" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "") {
		if u.User != nil {
			u.User = url.User("xxxxx")
		}
		if u.RawQuery != "" {
			u.RawQuery = "xxxxx"
		}
		return fail("aifUrl", "must be an origin (scheme://host[:port]), got %s", u)
	}
	origin, _, err := OriginKey(c.AifURL)
	if err != nil {
		return fail("aifUrl", "%v", err)
	}
	c.AifURL = origin // the state key and the HTTP client both use this one form
	if !nameRE.MatchString(c.AgentName) {
		return fail("agentName", "must match %s, got %q", nameRE, c.AgentName)
	}
	if c.Thread <= 0 {
		return fail("thread", "required, a positive thread id")
	}

	if c.SystemPromptFile != "" {
		if !filepath.IsAbs(c.SystemPromptFile) {
			c.SystemPromptFile = filepath.Join(dir, c.SystemPromptFile)
		}
		if c.SystemPrompt == "" {
			b, err := os.ReadFile(c.SystemPromptFile)
			if err != nil {
				return fail("systemPromptFile", "%v", err)
			}
			c.SystemPrompt = string(b)
		}
	}

	if c.Interval == 0 {
		c.Interval = 60
	}
	if c.Interval < 10 || c.Interval > 86400 {
		return fail("interval", "must be 10..86400 seconds, got %d", c.Interval)
	}

	c.TurnTimeout = 30 * time.Minute
	if tt.TurnTimeout != "" {
		if c.TurnTimeout, err = time.ParseDuration(tt.TurnTimeout); err != nil {
			return fail("turnTimeout", "not a Go duration (like 30m), got %q", tt.TurnTimeout)
		}
	}
	if c.TurnTimeout < time.Minute {
		return fail("turnTimeout", "must be at least 1m, got %s", c.TurnTimeout)
	}

	if c.Cwd == "" {
		c.Cwd = "."
	}
	if !filepath.IsAbs(c.Cwd) {
		c.Cwd = filepath.Join(dir, c.Cwd)
	}
	if fi, err := os.Stat(c.Cwd); err != nil || !fi.IsDir() {
		return fail("cwd", "%s is not a directory", c.Cwd)
	}

	if len(c.Operators) == 0 {
		c.Operators = []string{"TheRoot", "gatekeeper"}
	}
	return c, nil
}

// IsOperator reports whether name's SHUTDOWN and RESET count; names compare case-insensitively,
// as the server compares them.
func (c Config) IsOperator(name string) bool {
	for _, op := range c.Operators {
		if strings.EqualFold(op, name) {
			return true
		}
	}
	return false
}

// OriginKey normalizes aifURL to its origin (scheme and host lower-cased, default port explicit,
// path dropped: "https://example.com:443") and derives the state-directory key from it: scheme,
// host and port joined by "_", an IPv6 host's ':' spelled '-', and any other character outside
// [A-Za-z0-9.-] replaced by '_' ("https_example.com_443", "http_--1_18080" for [::1]).
func OriginKey(aifURL string) (origin, key string, err error) {
	u, err := url.Parse(aifURL)
	if err != nil {
		return "", "", err
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	switch {
	case scheme != "http" && scheme != "https":
		return "", "", fmt.Errorf("scheme must be http or https, got %q", aifURL)
	case u.Hostname() == "":
		return "", "", fmt.Errorf("no host in %q", aifURL)
	case port == "" && scheme == "http":
		port = "80"
	case port == "":
		port = "443"
	}
	host := strings.ToLower(u.Hostname())
	origin = scheme + "://" + net.JoinHostPort(host, port)
	safe := func(r rune) rune {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}
	key = scheme + "_" + strings.Map(safe, strings.ReplaceAll(host, ":", "-")) + "_" + port
	return origin, key, nil
}

// StateDir is the connector's state directory, <home>/.aif-connect/<agentName>@<key>.
func StateDir(home, agentName, key string) string {
	return filepath.Join(home, ".aif-connect", agentName+"@"+key)
}
