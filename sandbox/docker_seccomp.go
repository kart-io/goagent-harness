package sandbox

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed profiles/moby-default.json
var dockerBaseline []byte

// dockerProfile only removes permissions from the pinned Moby baseline.
func dockerProfile(p Policy) ([]byte, error) {
	if p.Mode == DangerFullAccess && p.Network == NetworkInherit {
		return append([]byte(nil), dockerBaseline...), nil
	}
	var profile map[string]any
	if err := json.Unmarshal(dockerBaseline, &profile); err != nil {
		return nil, err
	}
	if profile["defaultAction"] != "SCMP_ACT_ERRNO" {
		return nil, ErrUnavailable
	}
	var rules []any
	for _, entry := range profile["syscalls"].([]any) {
		rule := entry.(map[string]any)
		var names []any
		hasSocket := false
		for _, name := range rule["names"].([]any) {
			switch name {
			case "socket":
				hasSocket = true
			case "socketcall", "socketpair", "io_uring_setup":
			default:
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			copyRule := map[string]any{}
			for k, v := range rule {
				copyRule[k] = v
			}
			copyRule["names"] = names
			rules = append(rules, copyRule)
		}
		if hasSocket && p.Network == NetworkInherit {
			if rule["action"] != "SCMP_ACT_ALLOW" {
				return nil, ErrUnavailable
			}
			args, ok := rule["args"].([]any)
			if !ok || len(args) != 1 {
				return nil, fmt.Errorf("%w: unexpected socket baseline", ErrUnavailable)
			}
			arg := args[0].(map[string]any)
			if arg["index"] != float64(0) {
				return nil, ErrUnavailable
			}
			if arg["op"] == "SCMP_CMP_LT" && arg["value"] == float64(3) {
				for _, family := range []float64{0, 2} {
					copyRule := map[string]any{}
					for k, v := range rule {
						copyRule[k] = v
					}
					copyRule["args"] = []any{map[string]any{"index": 0, "value": family, "op": "SCMP_CMP_EQ"}}
					rules = append(rules, copyRule)
				}
			} else if arg["op"] == "SCMP_CMP_EQ" && arg["value"] != float64(1) {
				rules = append(rules, rule)
			} else {
				return nil, fmt.Errorf("%w: unexpected socket comparison", ErrUnavailable)
			}
		}
	}
	profile["syscalls"] = rules
	return json.Marshal(profile)
}
func writeDockerProfile(p Policy) (string, func(), error) {
	profile, err := dockerProfile(p)
	if err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp("", "harness-docker-seccomp-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err = f.Write(profile); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}
	if err = f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}
