package guard

import (
	"context"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

// An argv allow rule obeys rule priority (a higher-priority deny still wins)
// and rejects always-allow denylisted interpreters/shells.
func TestArgvAllowKeepsDenyAndDenylist(t *testing.T) {
	ic := intercept.New(nil)
	ic.SetRulesForTest([]db.InterceptRule{
		{ID: 1, Name: "no force push", Enabled: true, Priority: 50, MatchTarget: "tool_input", MatchType: "string",
			Pattern: "--force", Action: "deny", Message: "force push is forbidden"},
		{ID: 2, Name: "argv allow curl", Enabled: true, Priority: -1000, MatchTarget: "tool_input", MatchType: "argv",
			Pattern: `["curl"]`, Action: "allow"},
		{ID: 3, Name: "argv allow git push", Enabled: true, Priority: -1000, MatchTarget: "tool_input", MatchType: "argv",
			Pattern: `["git","push"]`, Action: "allow"},
		{ID: 4, Name: "argv allow python3", Enabled: true, Priority: 200, MatchTarget: "tool_input", MatchType: "argv",
			Pattern: `["python3"]`, Action: "allow"},
		{ID: 5, Name: "deny inline script", Enabled: true, Priority: 10, MatchTarget: "tool_input", MatchType: "string",
			Pattern: "inline-script-marker", Action: "deny", Message: "inline script denied"},
	})
	g := NewWithInterceptor(ic)
	pre := func(tool, input string) (bool, string) {
		t.Helper()
		block, msg, err := g.Hooks().PreToolUse(context.Background(), tool, []byte(input))
		if err != nil {
			t.Fatalf("PreToolUse(%s): %v", tool, err)
		}
		return block, msg
	}

	if block, msg := pre("Bash", `{"command":"git push --force origin"}`); !block || !strings.Contains(msg, "force push is forbidden") {
		t.Fatalf("deny over argv allow: block=%v msg=%q", block, msg)
	}
	if block, msg := pre("Bash", `{"command":"git push origin main"}`); block {
		t.Fatalf("argv allow git push blocked: %s", msg)
	}
	if block, msg := pre("Bash", `{"command":"curl https://app.example.com/"}`); block {
		t.Fatalf("argv allow curl blocked: %s", msg)
	}
	// Denylisted argv allow ["python3"] (even at priority 200) must not match,
	// so the lower-priority deny rule still catches the call.
	if block, msg := pre("Bash", `{"command":"python3 -c 'print(\"inline-script-marker\")'"}`); !block || !strings.Contains(msg, "inline script denied") {
		t.Fatalf("denylisted argv allow bypassed lower deny: block=%v msg=%q", block, msg)
	}
}
