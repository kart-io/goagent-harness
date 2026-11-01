package permissions

import (
	"fmt"
	"github.com/kart-io/goagent/tools/policy"
)

// RoleRules supplies optional tool rules, not identities, filesystem policies or external resource grants.
// Install with policy.Default=Ask; explicit Deny rules always take precedence.
func RoleRules(role string) ([]policy.Rule, error) {
	var rules []policy.Rule
	add := func(decision policy.Decision, names ...string) {
		for _, name := range names {
			rules = append(rules, policy.Rule{Tool: name, Decision: decision})
		}
	}
	switch role {
	case "reader", "reviewer", "coordinator":
		add(policy.Allow, "read_file", "glob", "grep")
		// git read tools and linter invoke subprocesses, so they must not become a hidden shell grant.
		add(policy.Deny, "write_file", "edit_file", "multi_edit", "delete_file", "run_command", "git_commit", "git_status", "git_diff", "git_log", "run_linter")
	case "coder":
		add(policy.Allow, "read_file", "glob", "grep", "write_file", "edit_file", "multi_edit", "delete_file")
		add(policy.Ask, "run_command", "git_status", "git_diff", "git_log", "git_commit", "run_linter")
		add(policy.Deny, "publish", "git_push", "send_message")
	case "publisher":
		// Publishing needs the host's resource-aware rules; this template grants no action automatically.
		add(policy.Ask, "publish", "git_push", "send_message", "git_commit", "run_command")
	default:
		return nil, fmt.Errorf("permissions: unknown role %q", role)
	}
	return rules, nil
}
