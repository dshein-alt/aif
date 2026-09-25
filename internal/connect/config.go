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
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Config is the connector's config file, resolved: paths are absolute, defaults are filled in and
// systemPrompt holds the ROLE text (read from systemPromptFile when systemPrompt is empty).
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
	TurnTimeout      time.Duration `json:"turnTimeout"` // a Go duration string in the file
	Cwd              string        `json:"cwd"`
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
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return Config{}, fmt.Errorf("config %s: config file must be mode 0600 (it holds the token; is %04o)", path, fi.Mode().Perm())
	}
	// The outer TurnTimeout shadows Config.TurnTimeout for encoding/json (shallower field wins).
	var raw struct {
		Config
		TurnTimeout string `json:"turnTimeout"`
	}
	if err := json.NewDecoder(f).Decode(&raw); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return fail(te.Field, "must be a JSON %s, got %s", te.Type, te.Value)
		}
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	c := raw.Config
	if o.Agent != "" {
		c.Agent = o.Agent
	}
	if o.Bin != "" {
		c.Bin = o.Bin
	}
	dir := filepath.Dir(path)

	for _, r := range []struct{ key, val string }{
		{"agent", c.Agent}, {"goal", c.Goal}, {"aifUrl", c.AifURL},
		{"agentName", c.AgentName}, {"agentToken", c.AgentToken},
	} {
		if strings.TrimSpace(r.val) == "" {
			return fail(r.key, "required")
		}
	}
	switch c.Agent {
	case "claude", "codex", "pi", "opencode":
	default:
		return fail("agent", "must be one of claude|codex|pi|opencode, got %q", c.Agent)
	}
	if _, _, err := OriginKey(c.AifURL); err != nil {
		return fail("aifUrl", "%v", err)
	}
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
	if raw.TurnTimeout != "" {
		if c.TurnTimeout, err = time.ParseDuration(raw.TurnTimeout); err != nil {
			return fail("turnTimeout", "not a Go duration (like 30m), got %q", raw.TurnTimeout)
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
