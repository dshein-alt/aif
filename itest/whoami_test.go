package itest

import (
	"context"
	"strings"
	"testing"

	"github.com/dshein-alt/aif/internal/harness"
)

func TestWhoAmIOnboarding(t *testing.T) {
	r := harness.New(t, false)
	for _, transport := range []string{"rest", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			name := "NewBot" + transport
			token := r.Issue(name)
			var identity map[string]any
			if transport == "rest" {
				identity = r.Client(token).Get("/api/whoami").MustOK().JSON()
			} else {
				identity = mcpPayload(t, mcp(r, token, "whoami", map[string]any{}, nil))
			}
			if identity["as"] != name || identity["msg"] != "Connected to AIF (AI Interaction Forum)." || num(identity["ok"]) != 1 {
				t.Fatalf("unexpected identity: %v", identity)
			}
			if len(identity) != 3 {
				t.Fatalf("identity should be compact, without token or ping metadata: %v", identity)
			}
			// Named invites keep their credential and generic op uses the same implementation.
			if got := r.Client(token).Op("whoami", nil).MustOK().Str("as"); got != name {
				t.Fatalf("repeat/generic identity = %q", got)
			}
		})
	}
	invite := r.Issue("")
	blocked := r.Client(invite).Get("/api/whoami")
	if blocked.Code != 403 || blocked.Str("err") != "claim_required" || !strings.Contains(blocked.Str("hint"), "/api/agents") {
		t.Fatalf("unnamed invite: %d %s", blocked.Code, blocked.Text())
	}
	failed := mcpResult(t, mcp(r, invite, "whoami", map[string]any{}, nil))
	if failed["isError"] != true {
		t.Fatalf("MCP unnamed invite must fail: %v", failed)
	}
	body := mcpErr(t, mcp(r, invite, "whoami", map[string]any{}, nil))
	if body["err"] != "claim_required" || !strings.Contains(body["hint"].(string), "register") {
		t.Fatalf("MCP claim guidance: %v", body)
	}
	registered := r.Client(invite).Post("/api/agents", map[string]any{"name": "ChosenName"}).MustOK()
	token := registered.Str("token")
	if r.Client(token).Get("/api/whoami").MustOK().Str("as") != "ChosenName" {
		t.Fatal("claimed identity was not returned")
	}
	if got := mcpPayload(t, mcp(r, token, "whoami", map[string]any{}, nil))["as"]; got != "ChosenName" {
		t.Fatalf("MCP claimed identity: %v", got)
	}
	if r.Admin.Get("/api/whoami").MustOK().Str("as") != "gatekeeper" {
		t.Fatal("gatekeeper identity missing")
	}
}

func TestWhoAmIRejectsInvalidOrMismatchedIdentity(t *testing.T) {
	r := harness.New(t, false)
	if got := r.Client("").Get("/api/whoami"); got.Code != 401 || got.Str("err") != "need_token" {
		t.Fatalf("missing token: %d %s", got.Code, got.Text())
	}
	if got := r.Client("invalid").Get("/api/whoami"); got.Code != 403 || got.Str("err") != "bad_token" {
		t.Fatalf("invalid token: %d %s", got.Code, got.Text())
	}
	agent := r.Join("Alice")
	if got := agent.H("X-Agent", "Bob").Get("/api/whoami"); got.Code != 403 || got.Str("err") != "token_agent_mismatch" {
		t.Fatalf("identity override: %d %s", got.Code, got.Text())
	}
	if got := mcpErr(t, mcp(r, r.Tokens["Alice"], "whoami", map[string]any{"agent": "Bob"}, nil)); got["err"] != "token_agent_mismatch" {
		t.Fatalf("MCP identity override: %v", got)
	}
	if _, err := r.Pool().Exec(context.Background(), "UPDATE tokens SET revoked = 1 WHERE self_token = $1", r.Tokens["Alice"]); err != nil {
		t.Fatal(err)
	}
	if got := agent.Get("/api/whoami"); got.Code != 403 || got.Str("err") != "token_revoked" {
		t.Fatalf("revoked identity: %d %s", got.Code, got.Text())
	}
}
