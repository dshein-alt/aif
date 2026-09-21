package tokens

import "testing"

func TestBoundNamePrefersTheIssuedName(t *testing.T) {
	named := map[string]any{"name": "bot"}
	for _, claimed := range []string{"bot", "Bot", "BOT"} {
		if got := BoundName(named, claimed); got != "bot" {
			t.Errorf("BoundName(named, %q) = %q, want %q", claimed, got, "bot")
		}
	}
	// An un-named invite has nothing bound, so the claimed name is the only input.
	if got := BoundName(map[string]any{"name": ""}, "bot"); got != "bot" {
		t.Errorf("BoundName(unnamed) = %q, want %q", got, "bot")
	}
	if got := BoundName(nil, "bot"); got != "bot" {
		t.Errorf("BoundName(nil) = %q, want %q", got, "bot")
	}
}

// The regression this guards: a named invite must hand back the very token the client was
// configured with, whatever case the claimer types, or the client's next request 403s.
func TestNamedInviteTokenSurvivesClaim(t *testing.T) {
	const salt = "test-salt"
	nonce := NewNonce()

	row := map[string]any{"name": "bot", "nonce": nonce}
	invite := DeriveToken(salt, "bot", nonce) // what Issue handed the client
	for _, claimed := range []string{"bot", "Bot", "BOT"} {
		got := DeriveToken(salt, BoundName(row, claimed), nonce) // what Claim writes back
		if got != invite {
			t.Errorf("claim as %q rewrote the token: %q, want %q", claimed, got, invite)
		}
	}

	// An un-named invite is the opposite contract: it is a claim ticket, and claiming it must
	// mint the name-derived token, so the client does have to be reconfigured.
	blank := map[string]any{"name": "", "nonce": nonce}
	unnamed := DeriveToken(salt, "", nonce)
	got := DeriveToken(salt, BoundName(blank, "bot"), nonce)
	if got == unnamed {
		t.Error("claiming an un-named invite must not keep the invite token")
	}
	if want := DeriveToken(salt, "bot", nonce); got != want {
		t.Errorf("un-named claim = %q, want the name-derived %q", got, want)
	}
}
