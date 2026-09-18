package itest

import (
	"encoding/json"
	"testing"

	"aif/internal/harness"
)

// mcpErr returns the {err,...} body from an isError MCP result (which carries JSON text, not
// structuredContent), mirroring how a real MCP client inspects failures.
func mcpErr(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	res := mcpResult(t, resp)
	if arr, ok := res["content"].([]any); ok && len(arr) > 0 {
		text := arr[0].(map[string]any)["text"].(string)
		var out map[string]any
		if err := json.Unmarshal([]byte(text), &out); err == nil {
			return out
		}
	}
	t.Fatalf("no error body in MCP result: %v", res)
	return nil
}

// mcpPayload returns the success payload of a tools/call result, accepting either the
// structuredContent object or the compact JSON text body (a real MCP client must do the same).
func mcpPayload(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	res := mcpResult(t, resp)
	if m, ok := res["structuredContent"].(map[string]any); ok {
		return m
	}
	if arr, ok := res["content"].([]any); ok && len(arr) > 0 {
		if text, _ := arr[0].(map[string]any)["text"].(string); text != "" {
			var out map[string]any
			if json.Unmarshal([]byte(text), &out) == nil {
				return out
			}
		}
	}
	t.Fatalf("no payload in MCP result: %v", res)
	return nil
}

// The scenario from the field: an agent gets only an invite token, pastes it into its MCP client
// config as-is, and must self-claim a name via the register tool before it can do anything.
func TestMCPInviteTokenMustBeClaimedBeforeUse(t *testing.T) {
	r := harness.New(t, false)
	invite := r.Issue("") // unbound invite, exactly what an admin hands out
	res := mcp(r, invite, "post", map[string]any{"subject": "sneaky", "b": "x"}, nil)
	eqStr(t, mcpErr(t, res)["err"].(string), "claim_required", "an unclaimed invite cannot post")

	// ping and skill are allowed pre-claim (so a client can orient itself), but nothing else.
	if isErr := mcpResult(t, mcp(r, invite, "ping", map[string]any{}, nil))["isError"]; isErr == true {
		t.Fatal("ping should be callable with an unclaimed invite")
	}
}

func TestMCPRegisterClaimsInviteAndFinalTokenWorks(t *testing.T) {
	r := harness.New(t, false)
	invite := r.Issue("")
	reg := mcpPayload(t, mcp(r, invite, "register", map[string]any{"name": "mcpprobe", "descr": "via mcp"}, nil))
	final, _ := reg["token"].(string)
	if final == "" {
		t.Fatalf("register returned no token: %v", reg)
	}
	eqStr(t, reg["name"].(string), "mcpprobe", "claimed name echoed")

	// The final token is name-bound: ping reports the claimed identity.
	ping := mcpPayload(t, mcp(r, final, "ping", map[string]any{}, nil))
	eqStr(t, ping["as"].(string), "mcpprobe", "final token authenticates as the claimed agent")

	// Claiming rotates the token (self_token becomes name-derived), so the spent invite string
	// no longer resolves - the MCP transport rejects it at auth time with a bare {err: bad_token}.
	again := mcp(r, invite, "register", map[string]any{"name": "second"}, nil)
	eqStr(t, again["err"].(string), "bad_token", "the spent invite string is dead after claim")
}

func TestMCPClaimedTokenCannotImpersonateAnotherName(t *testing.T) {
	r := harness.New(t, false)
	r.Join("alice") // create another real agent so the impersonation target exists
	alice := r.Tokens["alice"]

	// A claimed token that tries to act as someone else via the agent arg is rejected.
	res := mcpErr(t, mcp(r, alice, "post", map[string]any{"subject": "hi", "b": "x", "agent": "bob"}, nil))
	eqStr(t, res["err"].(string), "token_agent_mismatch", "a bound token cannot spoof another name")
}
