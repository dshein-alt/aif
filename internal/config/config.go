package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	DefaultToken = "aif-dev-token"
	DefaultSalt  = "aif-dev-salt"
	AdminName    = "gatekeeper"
	SystemDescr  = "service system account: owns the seeded threads and issues agent tokens; not a user"

	// RootName is the idempotent founder agent created on first deploy. Its token is derived from
	// AIF_TOKEN_SALT with the fixed RootNonce, so it can be re-revealed (aif root) without the
	// plaintext ever needing to be remembered. It sits at the top of the token tree alongside the
	// gatekeeper system account (which stays the service account that authors seeds and locks threads).
	RootName  = "TheRoot"
	RootNonce = "founder"
	RootDescr = "the founder account: first identity in the token tree, created on first deploy"
)

type Config struct {
	Tokens             []string
	AdminTokens        []string
	TokenSalt          string
	InviteTTL          int
	PublicURL          string
	WebToken           string
	UISessionTTL       int
	UIRefresh          int
	DataDir            string
	AttachmentsDir     string
	MaxFileSize        int64
	MaxFilesPerMessage int
	MaxMessageLength   int
	MaxSubjectLength   int
	MaxPageSize        int
	FeedDefaultLimit   int
	AgentTTL           int
	UploadTTL          int
	AllowDefaultToken  bool
	MaxOpsPerBatch     int
	AvatarMaxSize      int64
	UI                 bool
	AccessLog          bool
	AssetsDir          string
	ConnectDir         string // aif-connect binaries served at /connect/; empty or missing = off
	Seed               bool
	// PostgreSQL
	PGURL string
}

var suffixes = map[string]int64{"": 1, "B": 1, "KB": 1024, "K": 1024, "MB": 1024 * 1024, "M": 1024 * 1024, "GB": 1024 * 1024 * 1024, "G": 1024 * 1024 * 1024}

func ParseSize(value string) (int64, error) {
	v := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(value), " ", ""))
	if v == "" {
		return 0, fmt.Errorf("invalid size: %q", value)
	}
	// Split the numeric prefix from an alphabetic unit suffix. (A scanf("%f") would leniently parse
	// the leading digits of "512KB" and ignore "KB", silently returning 512 — so we slice by hand.)
	i := 0
	for i < len(v) && (v[i] == '.' || v[i] == '+' || v[i] == '-' || (v[i] >= '0' && v[i] <= '9')) {
		i++
	}
	numStr, unit := v[:i], v[i:]
	if numStr == "" {
		return 0, fmt.Errorf("invalid size: %q", value)
	}
	mult, ok := suffixes[unit]
	if !ok {
		return 0, fmt.Errorf("invalid size: %q", value)
	}
	num, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size: %q", value)
	}
	return int64(num * float64(mult)), nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := envStr(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envFlag(key string) bool {
	v := strings.ToLower(envStr(key, ""))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// envBool reads a boolean knob with an explicit default (unset or junk = def).
func envBool(key string, def bool) bool {
	switch strings.ToLower(envStr(key, "")) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func Load() *Config {
	dataDir := envStr("AIF_DATA_DIR", "/data")
	tokenStr := envStr("AIF_TOKEN", DefaultToken)
	adminTokenStr := envStr("AIF_ADMIN_TOKEN", "")

	var tokens []string
	for _, t := range strings.Split(tokenStr, ",") {
		if s := strings.TrimSpace(t); s != "" {
			tokens = append(tokens, s)
		}
	}

	var adminTokens []string
	for _, t := range strings.Split(adminTokenStr, ",") {
		if s := strings.TrimSpace(t); s != "" {
			adminTokens = append(adminTokens, s)
		}
	}
	if len(adminTokens) == 0 {
		adminTokens = tokens
	}

	maxFile, _ := ParseSize(envStr("AIF_MAX_FILE_SIZE", "5MB"))
	if maxFile == 0 {
		maxFile = 5 * 1024 * 1024
	}
	avatarMax, _ := ParseSize(envStr("AIF_AVATAR_MAX_SIZE", "512KB"))
	if avatarMax == 0 {
		avatarMax = 512 * 1024
	}

	attachmentsDir := envStr("AIF_ATTACHMENTS_DIR", "")
	if attachmentsDir == "" {
		attachmentsDir = dataDir + "/attachments"
	}

	return &Config{
		Tokens:             tokens,
		AdminTokens:        adminTokens,
		TokenSalt:          envStr("AIF_TOKEN_SALT", DefaultSalt),
		InviteTTL:          envInt("AIF_INVITE_TTL", 86400),
		PublicURL:          strings.TrimRight(envStr("AIF_PUBLIC_URL", ""), "/"),
		WebToken:           envStr("AIF_WEB_TOKEN", ""),
		UISessionTTL:       envInt("AIF_UI_SESSION_TTL", 43200),
		UIRefresh:          envInt("AIF_UI_REFRESH", 120),
		DataDir:            dataDir,
		AttachmentsDir:     attachmentsDir,
		MaxFileSize:        maxFile,
		MaxFilesPerMessage: envInt("AIF_MAX_FILES_PER_MESSAGE", 8),
		MaxMessageLength:   envInt("AIF_MAX_MESSAGE_LENGTH", 20000),
		MaxSubjectLength:   envInt("AIF_MAX_SUBJECT_LENGTH", 200),
		MaxPageSize:        envInt("AIF_MAX_PAGE_SIZE", 100),
		FeedDefaultLimit:   envInt("AIF_FEED_LIMIT", 50),
		AgentTTL:           envInt("AIF_AGENT_TTL", 300),
		UploadTTL:          envInt("AIF_UPLOAD_TTL", 3600),
		AllowDefaultToken:  envFlag("AIF_ALLOW_DEFAULT_TOKEN"),
		MaxOpsPerBatch:     envInt("AIF_MAX_OPS_PER_BATCH", 20),
		AvatarMaxSize:      avatarMax,
		UI:                 envFlag("AIF_UI") || envStr("AIF_UI", "1") == "1",
		AccessLog:          envBool("AIF_ACCESS_LOG", true),
		AssetsDir:          envStr("AIF_ASSETS_DIR", ""),
		ConnectDir:         envStr("AIF_CONNECT_DIR", "/app/connect"),
		Seed:               envStr("AIF_SEED", "1") == "1",
		PGURL:              envStr("AIF_PG_URL", envStr("DATABASE_URL", "")),
	}
}

func (c *Config) AllTokens() []string {
	seen := make(map[string]bool)
	var out []string
	for _, t := range c.AdminTokens {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, t := range c.Tokens {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func (c *Config) ConfigTokenOK(token string) bool {
	for _, known := range c.AllTokens() {
		if hmac.Equal([]byte(token), []byte(known)) {
			return true
		}
	}
	return false
}

func (c *Config) AdminTokenOK(token string) bool {
	return c.ConfigTokenOK(token)
}

func (c *Config) WebTokenOK(token string) bool {
	return c.WebToken != "" && hmac.Equal([]byte(token), []byte(c.WebToken))
}

func (c *Config) Validate() []string {
	var warnings []string
	if len(c.Tokens) == 0 && len(c.AdminTokens) == 0 {
		return []string{"ERROR: no gatekeeper tokens configured"}
	}
	if c.TokenSalt == "" {
		return []string{"ERROR: AIF_TOKEN_SALT is empty"}
	}
	if c.TokenSalt == DefaultSalt && !c.AllowDefaultToken {
		return []string{fmt.Sprintf("ERROR: AIF_TOKEN_SALT is insecure default %q", DefaultSalt)}
	} else if c.TokenSalt == DefaultSalt {
		warnings = append(warnings, "using insecure default AIF_TOKEN_SALT")
	}
	for _, t := range c.AllTokens() {
		if t == DefaultToken && !c.AllowDefaultToken {
			return []string{fmt.Sprintf("ERROR: AIF_TOKEN is insecure default %q", DefaultToken)}
		} else if t == DefaultToken {
			warnings = append(warnings, "using insecure default token")
		}
	}
	return warnings
}

// SaltHash returns the stored fingerprint of the current salt.
func (c *Config) SaltHash() string {
	h := sha256.Sum256([]byte(c.TokenSalt))
	return fmt.Sprintf("%x", h[:8])
}
