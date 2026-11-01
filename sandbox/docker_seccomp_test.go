package sandbox

import (
	"encoding/json"
	"testing"
)

func TestDockerProfileOnlyRestrictsBaseline(t *testing.T) {
	for _, network := range []Network{NetworkDeny, NetworkInherit} {
		data, err := dockerProfile(Policy{Mode: WorkspaceWrite, Network: network})
		if err != nil {
			t.Fatal(err)
		}
		var profile map[string]any
		if err := json.Unmarshal(data, &profile); err != nil {
			t.Fatal(err)
		}
		if profile["defaultAction"] != "SCMP_ACT_ERRNO" {
			t.Fatal("baseline weakened")
		}
		socketRules := 0
		for _, entry := range profile["syscalls"].([]any) {
			rule := entry.(map[string]any)
			for _, name := range rule["names"].([]any) {
				switch name {
				case "io_uring_setup", "socketcall", "socketpair":
					t.Fatal("socket bypass allowed")
				case "socket":
					socketRules++
					if network == NetworkDeny {
						t.Fatal("socket allowed in deny")
					}
					args := rule["args"].([]any)
					arg := args[0].(map[string]any)
					if arg["index"] != float64(0) || arg["op"] != "SCMP_CMP_EQ" || arg["value"] == float64(1) {
						t.Fatal("unix family could be allowed")
					}
				}
			}
		}
		if network == NetworkInherit && socketRules == 0 {
			t.Fatal("inherit socket rules lost")
		}
	}
}
