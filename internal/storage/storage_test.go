package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"aif/internal/config"
)

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{AttachmentsDir: t.TempDir(), MaxFileSize: 1 << 20}
}

func TestNewKeyIsHex32(t *testing.T) {
	k := NewKey()
	if len(k) != 32 || strings.TrimLeft(k, "0123456789abcdef") != "" {
		t.Fatalf("NewKey = %q, want 32 hex chars", k)
	}
	if NewKey() == k {
		t.Fatal("NewKey repeated the same value")
	}
}

func TestSaveReadRemoveRoundTrip(t *testing.T) {
	cfg := testCfg(t)
	key := NewKey()
	data := []byte("hello blob world")

	size, sum, err := Save(cfg, bytes.NewReader(data), key, cfg.MaxFileSize)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if size != int64(len(data)) {
		t.Errorf("size = %d, want %d", size, len(data))
	}
	exp := sha256.Sum256(data)
	if sum != hex.EncodeToString(exp[:]) {
		t.Errorf("sha mismatch: %q", sum)
	}

	back, err := ReadAll(cfg, key)
	if err != nil || !bytes.Equal(back, data) {
		t.Fatalf("ReadAll = %q, %v", back, err)
	}

	if !Remove(cfg, key) {
		t.Error("first Remove should report true")
	}
	if Remove(cfg, key) {
		t.Error("second Remove of a gone blob should report false")
	}
	if _, err := Open(cfg, key); err != ErrStorage {
		t.Errorf("Open of removed blob = %v, want ErrStorage", err)
	}
}

func TestSaveEnforcesSizeLimit(t *testing.T) {
	cfg := testCfg(t)
	key := NewKey()
	_, _, err := Save(cfg, strings.NewReader("way too big for the cap"), key, 4)
	if err != ErrTooLarge {
		t.Fatalf("Save over limit = %v, want ErrTooLarge", err)
	}
	// The partial write must have been cleaned up.
	if _, err := Open(cfg, key); err != ErrStorage {
		t.Errorf("partial blob left behind: %v", err)
	}
}

func TestRejectsMalformedKey(t *testing.T) {
	cfg := testCfg(t)
	for _, bad := range []string{"../../etc/passwd", "nothex", "deadbeef", "DEADBEEF0123456789abcdef01234567"} {
		if _, _, err := Save(cfg, strings.NewReader("x"), bad, cfg.MaxFileSize); err != ErrStorage {
			t.Errorf("Save with key %q = %v, want ErrStorage", bad, err)
		}
		if _, err := Open(cfg, bad); err != ErrStorage {
			t.Errorf("Open with key %q = %v, want ErrStorage", bad, err)
		}
		if Remove(cfg, bad) {
			t.Errorf("Remove with key %q should be false", bad)
		}
	}
}

func TestStatsCountsBlobs(t *testing.T) {
	cfg := testCfg(t)
	for i := 0; i < 3; i++ {
		if _, _, err := Save(cfg, strings.NewReader("abcdefgh"), NewKey(), cfg.MaxFileSize); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	count, total := Stats(cfg)
	if count != 3 {
		t.Errorf("Stats count = %d, want 3", count)
	}
	if total != 24 {
		t.Errorf("Stats total = %d, want 24", total)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[any]string{
		"/etc/passwd":         "passwd", // path traversal stripped to the basename
		"..\\..\\secrets.txt": "secrets.txt",
		"  a   b  \tc.txt  ":  "a b c.txt", // whitespace collapsed
		"":                    "file",      // empty falls back
		"   ":                 "file",
	}
	for in, want := range cases {
		if got := SanitizeName(in, 50); got != want {
			t.Errorf("SanitizeName(%q,50) = %q, want %q", in, got, want)
		}
	}
	if got := SanitizeName(strings.Repeat("x", 50), 10); got != strings.Repeat("x", 10) {
		t.Errorf("SanitizeName cap = %q", got)
	}
}
