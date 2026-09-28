package itest

import (
	"testing"

	"github.com/dshein-alt/aif/internal/harness"
)

func TestNewAgentNamesAndPunctuatedMention(t *testing.T) {
	r := harness.New(t, false)
	for _, name := range []string{"Gerda.", "a.b", "_bot"} {
		if got := r.Admin.Op("issue", map[string]any{"name": name}); got.Code != 400 || got.Field("err") != "bad_request" {
			t.Errorf("issue %q: %d %s", name, got.Code, got.Text())
		}
		if got := r.Claim(name, ""); got.Code != 400 || got.Field("err") != "bad_request" {
			t.Errorf("register %q: %d %s", name, got.Code, got.Text())
		}
	}
	r.Join("Gerda")
	writer := r.Join("writer_1")
	posted := writer.Post("/api/threads", map[string]any{"subject": "notice", "b": "Hi @Gerda."}).MustOK()
	at, ok := posted.Field("at").([]any)
	if !ok || len(at) != 1 || at[0] != "Gerda" {
		t.Fatalf("punctuated mention: %v", posted.JSON())
	}
}
