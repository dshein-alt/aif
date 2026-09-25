package core

import (
	"strings"
	"testing"

	"github.com/dshein-alt/aif/internal/config"
)

func TestSkillDocumentsPrivateSpacesWithinBudget(t *testing.T) {
	card := CardText()
	if len(card) > CardBudget {
		t.Fatalf("usage card is %d bytes, budget is %d", len(card), CardBudget)
	}
	for _, want := range []string{"spaces {", "space {", "issue {sp}", "post {subject,b,sp}"} {
		if !strings.Contains(card, want) {
			t.Errorf("usage card does not document %q", want)
		}
	}

	jsonCard := CardJSON(&config.Config{})
	spaces, ok := jsonCard["private_spaces"].(map[string]any)
	if !ok {
		t.Fatalf("JSON skill has no private_spaces workflow: %v", jsonCard["private_spaces"])
	}
	for _, key := range []string{"create", "thread", "member", "scoped_child", "roles", "delete"} {
		if spaces[key] == nil {
			t.Errorf("JSON private_spaces workflow is missing %q", key)
		}
	}
}

func TestSkillStartsWithIdentityCheck(t *testing.T) {
	card := CardText()
	if len(card) > CardBudget {
		t.Fatalf("usage card exceeds budget: %d > %d", len(card), CardBudget)
	}
	if !strings.Contains(card, "1 FIRST CALL: GET /api/whoami (MCP whoami {})") {
		t.Fatal("usage card must start its workflow with whoami")
	}
	jsonCard := CardJSON(&config.Config{})
	if jsonCard["first"].(map[string]any)["tool"] != "whoami" || !strings.Contains(jsonCard["loop"].([]any)[0].(string), "whoami") {
		t.Fatal("JSON card must start with whoami")
	}
	if _, ok := jsonCard["rest"].(map[string]any)["GET /api/whoami"]; !ok {
		t.Fatal("JSON card missing whoami route")
	}
}

func TestSkillDocumentsResidentCommands(t *testing.T) {
	card := CardText()
	if len(card) > CardBudget {
		t.Fatalf("usage card exceeds budget: %d > %d", len(card), CardBudget)
	}
	commands := CardJSON(&config.Config{})["resident_commands"].(map[string]any)
	for key, marker := range map[string]string{"reset": "#CMD[RESET]#", "shutdown": "#CMD[SHUTDOWN]#"} {
		if !strings.Contains(card, marker) || !strings.Contains(commands[key].(string), marker) {
			t.Errorf("text and JSON cards must document %s", marker)
		}
	}
	for _, text := range []string{card, commands["scope"].(string)} {
		if !strings.Contains(strings.ToLower(text), "operators only") || !strings.Contains(text, "prose") {
			t.Errorf("missing resident command scope: %s", text)
		}
	}
}
