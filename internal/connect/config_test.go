package connect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const minimal = `"agent":"claude","goal":"g","aifUrl":"http://h:1","agentName":"mybot","agentToken":"aif_x","thread":8`

func writeConfig(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadErrors(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"no agent", `{"goal":"g","aifUrl":"http://h","agentName":"a","agentToken":"t","thread":1}`, "agent: required"},
		{"bad agent", `{` + minimal + `,"agent":"gpt"}`, "agent: must be one of claude|codex|pi|opencode"},
		{"no goal", `{"agent":"pi","aifUrl":"http://h","agentName":"a","agentToken":"t","thread":1}`, "goal: required"},
		{"no aifUrl", `{"agent":"pi","goal":"g","agentName":"a","agentToken":"t","thread":1}`, "aifUrl: required"},
		{"bad aifUrl", `{` + minimal + `,"aifUrl":"ftp://h"}`, "aifUrl: scheme must be http or https"},
		{"no agentName", `{"agent":"pi","goal":"g","aifUrl":"http://h","agentToken":"t","thread":1}`, "agentName: required"},
		{"bad agentName", `{` + minimal + `,"agentName":"../x"}`, "agentName: must match"},
		{"no agentToken", `{"agent":"pi","goal":"g","aifUrl":"http://h","agentName":"a","thread":1}`, "agentToken: required"},
		{"no thread", `{"agent":"pi","goal":"g","aifUrl":"http://h","agentName":"a","agentToken":"t"}`, "thread: required"},
		{"thread type", `{` + minimal + `,"thread":"8"}`, ": thread: must be a JSON int64, got string"},
		{"operators type", `{` + minimal + `,"operators":"David"}`, ": operators: must be a JSON []string, got string"},
		{"timeout type", `{` + minimal + `,"turnTimeout":30}`, ": turnTimeout: must be a JSON string, got number"},
		{"top-level array", `[]`, ": must be a JSON object, got array"},
		{"trailing garbage", `{` + minimal + `} x`, "invalid character 'x' after top-level value"},
		{"blank goal", `{` + minimal + `,"goal":"  "}`, "goal: required"},
		{"cwd missing", `{` + minimal + `,"cwd":"nope"}`, "nope is not a directory"},
		{"cwd a file", `{` + minimal + `,"cwd":"c.json"}`, "c.json is not a directory"},
		{"interval low", `{` + minimal + `,"interval":9}`, "interval: must be 10..86400"},
		{"interval high", `{` + minimal + `,"interval":86401}`, "interval: must be 10..86400"},
		{"timeout syntax", `{` + minimal + `,"turnTimeout":"30"}`, "turnTimeout: not a Go duration"},
		{"timeout low", `{` + minimal + `,"turnTimeout":"59s"}`, "turnTimeout: must be at least 1m"},
		{"prompt file missing", `{` + minimal + `,"systemPromptFile":"nope.md"}`, "systemPromptFile:"},
		{"not json", `{`, "unexpected end of JSON input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tc.body, 0o600), Overrides{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	p := writeConfig(t, `{`+minimal+`}`, 0o600)
	c, err := LoadConfig(p, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval != 60 || c.TurnTimeout != 30*time.Minute || c.Cwd != filepath.Dir(p) || c.SystemPrompt != "" {
		t.Fatalf("defaults: %+v", c)
	}
	if strings.Join(c.Operators, ",") != "TheRoot,gatekeeper" {
		t.Fatalf("operators = %v", c.Operators)
	}
	if !c.IsOperator("theroot") || !c.IsOperator("GATEKEEPER") || c.IsOperator("mybot") {
		t.Fatal("IsOperator is not case-insensitive over the defaults")
	}
	if c.AifURL != "http://h:1" || c.Ignored != nil {
		t.Fatalf("aifUrl %q, ignored %v", c.AifURL, c.Ignored)
	}
	if b, _ := json.Marshal(c); strings.Contains(string(b), "urnTimeout") || strings.Contains(string(b), "gnored") {
		t.Fatalf("marshaled Config carries internal fields: %s", b)
	}
}

func TestLoadNormalizes(t *testing.T) {
	body := `{"agent":" pi ","goal":" g ","aifUrl":"HTTPS://Example.COM/","agentName":" mybot ","agentToken":" aif_x ",` +
		`"bin":" /x/pi ","thread":8,"mcpUrl":"https://example.com/mcp","provider":"x","AGENTNAME":"mybot"}`
	c, err := LoadConfig(writeConfig(t, body, 0o600), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if c.AifURL != "https://example.com:443" {
		t.Fatalf("aifUrl = %q", c.AifURL)
	}
	if c.Agent != "pi" || c.Goal != "g" || c.AgentName != "mybot" || c.AgentToken != "aif_x" || c.Bin != "/x/pi" {
		t.Fatalf("not trimmed: %+v", c)
	}
	if strings.Join(c.Ignored, ",") != "mcpUrl,provider" {
		t.Fatalf("ignored = %v", c.Ignored)
	}
}

func TestLoadAifURLOrigin(t *testing.T) {
	for _, u := range []string{
		"http://h:1/mcp", "http://aif_secret@h:1", "http://u:aif_secret@h:1", "http://h:1?token=aif_secret",
		"http://h:1/?x", "http://h:1?", "http://h:1#frag",
	} {
		_, err := LoadConfig(writeConfig(t, `{`+minimal+`,"aifUrl":"`+u+`"}`, 0o600), Overrides{})
		if err == nil || !strings.Contains(err.Error(), "aifUrl: must be an origin (scheme://host[:port]), got ") {
			t.Errorf("%s: err = %v", u, err)
		} else if strings.Contains(err.Error(), "aif_secret") {
			t.Errorf("%s: token not redacted: %v", u, err)
		}
	}
}

func TestLoadValues(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "role.md"), []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "c.json")
	body := `{` + minimal + `,"systemPromptFile":"role.md","interval":10,"turnTimeout":"1m","cwd":"work","operators":["David"],"bin":"/x/claude"}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if c.SystemPrompt != "from file" || c.SystemPromptFile != filepath.Join(dir, "role.md") {
		t.Fatalf("prompt file: %q %q", c.SystemPrompt, c.SystemPromptFile)
	}
	if c.Interval != 10 || c.TurnTimeout != time.Minute || c.Cwd != filepath.Join(dir, "work") || c.Bin != "/x/claude" {
		t.Fatalf("values: %+v", c)
	}
	if !c.IsOperator("david") || c.IsOperator("TheRoot") {
		t.Fatalf("operators = %v", c.Operators)
	}

	// systemPrompt wins over the file (the file is not even read).
	body = `{` + minimal + `,"systemPrompt":"inline","systemPromptFile":"missing.md"}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = LoadConfig(p, Overrides{}); err != nil || c.SystemPrompt != "inline" {
		t.Fatalf("systemPrompt: %q, %v", c.SystemPrompt, err)
	}

	// The flags win over the file, and an overridden agent is validated too.
	c, err = LoadConfig(p, Overrides{Agent: "codex", Bin: "/y/codex"})
	if err != nil || c.Agent != "codex" || c.Bin != "/y/codex" {
		t.Fatalf("overrides: %+v, %v", c, err)
	}
	if _, err = LoadConfig(p, Overrides{Agent: "nope"}); err == nil || !strings.Contains(err.Error(), "agent: must be one of") {
		t.Fatalf("bad override: %v", err)
	}
}

func TestLoadMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode check is Unix only")
	}
	for _, m := range []os.FileMode{0o640, 0o604, 0o644} {
		_, err := LoadConfig(writeConfig(t, `{`+minimal+`}`, m), Overrides{})
		if err == nil || !strings.Contains(err.Error(), "config file must be mode 0600") {
			t.Fatalf("mode %o: err = %v", m, err)
		}
	}
	if _, err := LoadConfig(writeConfig(t, `{`+minimal+`}`, 0o400), Overrides{}); err != nil {
		t.Fatalf("0400: %v", err)
	}
}

func TestOriginKey(t *testing.T) {
	for _, tc := range []struct{ in, origin, key string }{
		{"https://example.com", "https://example.com:443", "https_example.com_443"},
		{"https://Example.COM:443/", "https://example.com:443", "https_example.com_443"},
		{"HTTP://example.com", "http://example.com:80", "http_example.com_80"},
		{"http://aif.example:18080/mcp", "http://aif.example:18080", "http_aif.example_18080"},
		{"http://[::1]:18080/mcp", "http://[::1]:18080", "http_--1_18080"},
	} {
		origin, key, err := OriginKey(tc.in)
		if err != nil || origin != tc.origin || key != tc.key {
			t.Errorf("OriginKey(%q) = %q, %q, %v; want %q, %q", tc.in, origin, key, err, tc.origin, tc.key)
		}
	}
	for _, bad := range []string{"", "example.com", "ftp://h", "http://", "http://h:port"} {
		if _, _, err := OriginKey(bad); err == nil {
			t.Errorf("OriginKey(%q): no error", bad)
		}
	}
	if got, want := StateDir("/home/u", "mybot", "https_example.com_443"), filepath.Join("/home/u", ".aif-connect", "mybot@https_example.com_443"); got != want {
		t.Fatalf("StateDir = %q, want %q", got, want)
	}
}
