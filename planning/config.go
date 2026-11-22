package planning

import (
	"fmt"
	"math"
	"slices"
	"strings"

	agentErrors "github.com/kart-io/goagent/errors"
)

// NormalizeConfig applies zero-value defaults, validates planning bounds and
// the trusted member directory, and returns a copy owned by the caller.
func NormalizeConfig(cfg Config) (Config, error) {
	for _, limit := range []struct {
		name     string
		value    *int
		fallback int
	}{
		{"Parallelism", &cfg.Parallelism, 1},
		{"MaxTasks", &cfg.MaxTasks, 64},
		{"MaxWaves", &cfg.MaxWaves, 256},
		{"MaxReplans", &cfg.MaxReplans, 3},
		{"MaxTaskAttempts", &cfg.MaxTaskAttempts, 3},
		{"MaxGoalRounds", &cfg.MaxGoalRounds, 4},
		{"MaxArtifactBytes", &cfg.MaxArtifactBytes, 65536},
		{"MaxArtifactTotalBytes", &cfg.MaxArtifactTotalBytes, 1048576},
		{"MaxStateBytes", &cfg.MaxStateBytes, 8388608},
	} {
		if *limit.value < 0 {
			return Config{}, configError("%s must be nonnegative", limit.name)
		}
		if *limit.value == 0 {
			*limit.value = limit.fallback
		}
	}

	// 先从可用步数扣除各项额度，再计算最终公式，避免中间结果溢出。
	remaining := (math.MaxInt - 12) / 4
	for _, count := range []int{cfg.MaxGoalRounds, cfg.MaxWaves, cfg.MaxReplans} {
		if count > remaining {
			return Config{}, configError("workflow step limit overflows int")
		}
		remaining -= count
	}
	if cfg.MaxTasks > math.MaxInt/cfg.MaxTaskAttempts {
		return Config{}, configError("attempt capacity overflows int")
	}

	if strings.TrimSpace(cfg.Version) == "" {
		return Config{}, configError("Version is required")
	}
	if len(cfg.Members) == 0 {
		return Config{}, configError("at least one member is required")
	}
	ids := make(map[string]struct{}, len(cfg.Members))
	cfg.Members = slices.Clone(cfg.Members)
	for i, member := range cfg.Members {
		if err := validateMemberConfig(member); err != nil {
			return Config{}, err
		}
		if _, found := ids[member.ID]; found {
			return Config{}, configError("duplicate member ID %q", member.ID)
		}
		ids[member.ID] = struct{}{}
		cfg.Members[i].Claims = slices.Clone(member.Claims)
		cfg.Members[i].Capabilities = slices.Clone(member.Capabilities)
	}
	cfg.RequiredGoalParameters = slices.Clone(cfg.RequiredGoalParameters)
	return cfg, nil
}

// maxSteps 仅用于已通过 NormalizeConfig 校验的配置。
func (cfg Config) maxSteps() int {
	return 12 + 4*cfg.MaxGoalRounds + 4*cfg.MaxWaves + 4*cfg.MaxReplans
}

func validateMemberConfig(member Member) error {
	if strings.TrimSpace(member.ID) == "" {
		return configError("member ID is required")
	}
	if strings.TrimSpace(member.Version) == "" || strings.TrimSpace(member.ManifestDigest) == "" {
		return configError("member %q requires Version and ManifestDigest", member.ID)
	}
	if member.EffectClass != "read_only" && member.EffectClass != "write" {
		return configError("member %q has invalid EffectClass %q", member.ID, member.EffectClass)
	}

	seen := make(map[string]struct{}, len(member.Claims))
	hasWrite := false
	for _, claim := range member.Claims {
		if strings.TrimSpace(claim.Key) == "" {
			return configError("member %q has a resource claim without a key", member.ID)
		}
		if claim.Mode != "read" && claim.Mode != "write" {
			return configError("member %q has invalid resource claim mode %q", member.ID, claim.Mode)
		}
		if _, found := seen[claim.Key]; found {
			return configError("member %q repeats resource claim %q", member.ID, claim.Key)
		}
		seen[claim.Key] = struct{}{}
		hasWrite = hasWrite || claim.Mode == "write"
	}
	if hasWrite != (member.EffectClass == "write") {
		return configError("member %q has resource claims inconsistent with EffectClass %q", member.ID, member.EffectClass)
	}
	return nil
}

func configError(format string, args ...any) error {
	return agentErrors.NewInvalidConfigError("planning", "normalize_config", fmt.Sprintf(format, args...))
}
