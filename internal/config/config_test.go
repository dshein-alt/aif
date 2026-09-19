package config

import (
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	ok := map[string]int64{
		"1024":   1024,
		"0":      0,
		"512KB":  512 * 1024,
		"5MB":    5 * 1024 * 1024,
		"2G":     2 * 1024 * 1024 * 1024,
		"1K":     1024,
		"800B":   800,
		" 2 GB ": 2 * 1024 * 1024 * 1024,
		"1.5mb":  1572864, // 1.5 * 1024 * 1024, rounded down to an int64
		"5 mb":   5 * 1024 * 1024,
		"3":      3,
	}
	for in, want := range ok {
		got, err := ParseSize(in)
		if err != nil {
			t.Errorf("ParseSize(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "   ", "nonsense", "MB", "abc", "12XB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) expected an error", bad)
		}
	}
}

func TestEnvHelpers(t *testing.T) {
	t.Setenv("AIF_T_STR", "hello")
	if got := envStr("AIF_T_STR", "def"); got != "hello" {
		t.Errorf("envStr present = %q", got)
	}
	if got := envStr("AIF_T_ABSENT", "def"); got != "def" {
		t.Errorf("envStr absent = %q", got)
	}

	t.Setenv("AIF_T_INT", "42")
	if got := envInt("AIF_T_INT", 7); got != 42 {
		t.Errorf("envInt = %d", got)
	}
	if got := envInt("AIF_T_ABSENT", 7); got != 7 {
		t.Errorf("envInt absent default = %d", got)
	}
	t.Setenv("AIF_T_INT", "not-a-number")
	if got := envInt("AIF_T_INT", 7); got != 7 {
		t.Errorf("envInt invalid should fall back to default, got %d", got)
	}

	for _, truthy := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv("AIF_T_FLAG", truthy)
		if !envFlag("AIF_T_FLAG") {
			t.Errorf("envFlag(%q) = false, want true", truthy)
		}
	}
	for _, falsy := range []string{"0", "false", "off", "", "maybe"} {
		t.Setenv("AIF_T_FLAG", falsy)
		if envFlag("AIF_T_FLAG") {
			t.Errorf("envFlag(%q) = true, want false", falsy)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	// Isolate from any ambient AIF_* environment.
	for _, k := range []string{"AIF_TOKEN", "AIF_ADMIN_TOKEN", "AIF_TOKEN_SALT", "AIF_SEED", "AIF_UI", "AIF_MAX_FILE_SIZE", "AIF_AVATAR_MAX_SIZE", "AIF_DATA_DIR", "AIF_ATTACHMENTS_DIR", "AIF_PG_URL", "DATABASE_URL"} {
		t.Setenv(k, "")
	}
	c := Load()
	if len(c.Tokens) != 1 || c.Tokens[0] != DefaultToken {
		t.Errorf("default token not applied: %+v", c.Tokens)
	}
	if c.TokenSalt != DefaultSalt {
		t.Errorf("default salt not applied: %q", c.TokenSalt)
	}
	if c.MaxFileSize != 5*1024*1024 {
		t.Errorf("default max file = %d", c.MaxFileSize)
	}
	if c.AvatarMaxSize != 512*1024 {
		t.Errorf("default avatar max = %d", c.AvatarMaxSize)
	}
	if c.DataDir != "/data" || c.AttachmentsDir != "/data/attachments" {
		t.Errorf("default dirs wrong: %q / %q", c.DataDir, c.AttachmentsDir)
	}
	if !c.UI {
		t.Error("UI should default to on")
	}
	if !c.Seed {
		t.Error("Seed should default to on")
	}
	// admin tokens default to the same set as tokens.
	if len(c.AdminTokens) != 1 || c.AdminTokens[0] != DefaultToken {
		t.Errorf("admin tokens default wrong: %+v", c.AdminTokens)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("AIF_TOKEN", "t1, t2 ,t3")
	t.Setenv("AIF_ADMIN_TOKEN", "root-a,root-b")
	t.Setenv("AIF_TOKEN_SALT", "salty")
	t.Setenv("AIF_MAX_FILE_SIZE", "1MB")
	t.Setenv("AIF_AVATAR_MAX_SIZE", "64KB")
	t.Setenv("AIF_ATTACHMENTS_DIR", "/tmp/att")
	t.Setenv("AIF_INVITE_TTL", "60")
	t.Setenv("AIF_UI", "off")
	t.Setenv("AIF_SEED", "0")
	t.Setenv("AIF_DATA_DIR", "/srv/aif")
	t.Setenv("AIF_PUBLIC_URL", "https://aif.example/")
	t.Setenv("DATABASE_URL", "postgres://fallback")
	c := Load()

	if len(c.Tokens) != 3 || c.Tokens[1] != "t2" {
		t.Errorf("token split/trim wrong: %+v", c.Tokens)
	}
	if len(c.AdminTokens) != 2 || c.AdminTokens[0] != "root-a" {
		t.Errorf("admin split wrong: %+v", c.AdminTokens)
	}
	if c.MaxFileSize != 1024*1024 || c.AvatarMaxSize != 64*1024 {
		t.Errorf("size overrides wrong: %d / %d", c.MaxFileSize, c.AvatarMaxSize)
	}
	if c.AttachmentsDir != "/tmp/att" {
		t.Errorf("attachments override ignored: %q", c.AttachmentsDir)
	}
	if c.UI || c.Seed {
		t.Errorf("off switches not honoured: UI=%v Seed=%v", c.UI, c.Seed)
	}
	if c.PublicURL != "https://aif.example" {
		t.Errorf("public url trailing slash not trimmed: %q", c.PublicURL)
	}
	if c.PGURL != "postgres://fallback" {
		t.Errorf("DATABASE_URL fallback ignored: %q", c.PGURL)
	}
	// AIF_PG_URL wins over DATABASE_URL.
	t.Setenv("AIF_PG_URL", "postgres://primary")
	if Load().PGURL != "postgres://primary" {
		t.Error("AIF_PG_URL should take precedence over DATABASE_URL")
	}
}

func TestTokenSetsAndChecks(t *testing.T) {
	c := &Config{Tokens: []string{"a", "b", "a"}, AdminTokens: []string{"a", "c"}, WebToken: "web"}
	got := strings.Join(c.AllTokens(), ",")
	if got != "a,c,b" { // admin tokens first, deduped, then remaining tokens
		t.Errorf("AllTokens order/dedup = %q", got)
	}
	if !c.ConfigTokenOK("b") || !c.AdminTokenOK("c") {
		t.Error("token membership checks failed")
	}
	if c.ConfigTokenOK("nope") || c.WebTokenOK("nope") {
		t.Error("negative token checks failed")
	}
	if !c.WebTokenOK("web") {
		t.Error("web token not accepted")
	}
	if (&Config{}).WebTokenOK("") {
		t.Error("empty web token must never validate")
	}
}

func TestValidate(t *testing.T) {
	if w := (&Config{}).Validate(); len(w) == 0 || w[0][:5] != "ERROR" {
		t.Errorf("empty config should be a hard error, got %v", w)
	}
	noSalt := (&Config{Tokens: []string{"x"}, TokenSalt: ""}).Validate()
	if len(noSalt) == 0 || noSalt[0][:5] != "ERROR" {
		t.Errorf("empty salt should error, got %v", noSalt)
	}
	// Insecure defaults are hard errors unless explicitly allowed.
	def := (&Config{Tokens: []string{DefaultToken}, TokenSalt: DefaultSalt}).Validate()
	if len(def) == 0 || def[0][:5] != "ERROR" {
		t.Errorf("insecure defaults should error by default, got %v", def)
	}
	allowed := (&Config{Tokens: []string{DefaultToken}, TokenSalt: DefaultSalt, AllowDefaultToken: true}).Validate()
	if len(allowed) == 0 || allowed[0][:5] == "ERROR" {
		t.Errorf("with AllowDefaultToken, defaults should only warn, got %v", allowed)
	}
	good := (&Config{Tokens: []string{"real-token"}, TokenSalt: "real-salt"}).Validate()
	if len(good) != 0 {
		t.Errorf("a sound config should produce no warnings, got %v", good)
	}
}

func TestSaltHashDeterministic(t *testing.T) {
	a := (&Config{TokenSalt: "same"}).SaltHash()
	b := (&Config{TokenSalt: "same"}).SaltHash()
	c := (&Config{TokenSalt: "diff"}).SaltHash()
	if a != b || a == c || a == "" {
		t.Fatalf("SaltHash not deterministic/discriminating: %q %q %q", a, b, c)
	}
}
