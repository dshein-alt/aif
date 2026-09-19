package itest

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/harness"
)

// runInitCapture applies db.Init on the rig's pool with an overridden salt and captures whatever
// the server logs while doing so.
func runInitCapture(t *testing.T, r *harness.Rig, salt string) string {
	t.Helper()
	c := *r.Cfg
	c.TokenSalt = salt
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	err := db.Init(context.Background(), r.Pool(), &c)
	log.SetOutput(prev)
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	return buf.String()
}

// TestSaltChangeIsWarnedOnlyWhenTokensExist covers the startup guard: a fingerprint is recorded
// quietly on first start, and a later salt change is only alarming once there are agent tokens that
// the change would silently invalidate.
func TestSaltChangeIsWarnedOnlyWhenTokensExist(t *testing.T) {
	r := harness.New(t, false)

	// No agent tokens yet (only the gatekeeper system account exists): a salt change is silent.
	if out := runInitCapture(t, r, "some-other-salt"); out != "" {
		t.Fatalf("expected silence with no tokens, got: %q", out)
	}

	// Issue an agent token (there is now something the salt change would invalidate).
	r.Issue("")

	// Same salt: still silent (fingerprint matches).
	if out := runInitCapture(t, r, harness.Salt); out != "" {
		t.Fatalf("expected silence with matching salt, got: %q", out)
	}

	// Changed salt with a live token: warn, naming the knob and the consequence.
	out := runInitCapture(t, r, "some-other-salt")
	contains(t, out, "AIF_TOKEN_SALT", "salt warning names the knob")
	contains(t, out, "INVALID", "salt warning states the consequence")

	// It keeps warning until resolved, and restoring the salt restores silence.
	out2 := runInitCapture(t, r, "still-not-the-salt")
	contains(t, out2, "AIF_TOKEN_SALT", "keeps warning on the next start")
	if out := runInitCapture(t, r, harness.Salt); out != "" {
		t.Fatalf("restoring the salt should silence the warning, got: %q", out)
	}
}

// TestSaltFingerprintRecordedOnce checks the fingerprint is written on first start and not rewritten
// on a mismatch (which is why the warning persists rather than silently re-basing on the new salt).
func TestSaltFingerprintRecordedOnce(t *testing.T) {
	r := harness.New(t, false)
	r.Issue("")

	stored, ok := db.GetMeta(context.Background(), r.Pool(), "salt.sha")
	if !ok || stored == "" {
		t.Fatal("salt.sha fingerprint should be recorded on first init")
	}
	if want := r.Cfg.SaltHash(); stored != want {
		// sanity: the stored fingerprint matches the rig's own salt
		t.Fatalf("stored fingerprint %q does not match rig salt hash %q", stored, want)
	}

	// A mismatched init must NOT overwrite the stored (original) fingerprint.
	runInitCapture(t, r, "attacker-salt")
	after, _ := db.GetMeta(context.Background(), r.Pool(), "salt.sha")
	if after != stored {
		t.Fatalf("salt fingerprint was overwritten on mismatch (%q -> %q); the warning must persist", stored, after)
	}
	if strings.Contains(after, "attacker") {
		t.Fatal("stored fingerprint should still reflect the original salt")
	}
}
