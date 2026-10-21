package config

import (
	"strings"
	"testing"
)

const validConfig = `
owner: jon
people:
  jon: "+15550000001"
  jac: "+15550000002"
agents:
  budget:
    token: budget-token
    members: { jon: write, jac: read }
    history: 2
  garden:
    token: garden-token
    members: { jon: write }
mcp_token: mcp-token
`

func TestParseAcceptsAValidConfig(t *testing.T) {
	if _, err := Parse([]byte(validConfig)); err != nil {
		t.Fatal(err)
	}
}

func TestMentioned(t *testing.T) {
	c, _ := Parse([]byte(validConfig))
	for text, want := range map[string]string{
		"@budget balance?":       "budget",
		"hi @Budget and @garden": "budget garden",
		"@budgets":               "",
		"mail me@budget.com":     "",
	} {
		if got := strings.Join(c.Mentioned(text), " "); got != want {
			t.Errorf("Mentioned(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestConfigRejectsBadValues(t *testing.T) {
	for _, bad := range []string{
		strings.Replace(validConfig, "owner: jon", "owner: pat", 1),
		strings.Replace(validConfig, `jac: "+15550000002"`, `jac: "5550000002"`, 1),
		strings.Replace(validConfig, "history: 2", "history: 21", 1),
		strings.Replace(validConfig, "jon: write, jac: read", "jon: write, jac: admin", 1),
		strings.Replace(validConfig, "garden-token", "budget-token", 1),
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted a bad config:\n%s", bad)
		}
	}
}
