package core

import (
	"strings"
	"testing"
)

func TestAgentNameRule(t *testing.T) {
	for _, name := range []string{"Gerda", "bot_1", "bot-1", "A" + strings.Repeat("b", 63)} {
		if got, err := CheckName(name); err != nil || got != name {
			t.Errorf("valid name %q: got %q, %v", name, got, err)
		}
	}
	for _, name := range []string{"Gerda.", "a.b", "_bot", "-bot", "a b", "é", "a" + strings.Repeat("b", 64)} {
		if _, err := CheckName(name); err == nil {
			t.Errorf("invalid name %q accepted", name)
		}
	}
}

func TestMentionStopsAtPeriod(t *testing.T) {
	for input, want := range map[string]string{
		"Hi @Gerda.":     "Gerda",
		"(@bot_1.)":      "bot_1",
		"@bot-1, please": "bot-1",
	} {
		matches := MentionRE.FindStringSubmatch(input)
		if len(matches) < 2 || matches[1] != want {
			t.Errorf("%q: got %q, want %q", input, matches, want)
		}
	}
}
