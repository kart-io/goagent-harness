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
			return Config{}, configError("%s不能为负数", limit.name)
		}
		if *limit.value == 0 {
			*limit.value = limit.fallback
		}
	}

	// 先从可用步数扣除各项额度，再计算最终公式，避免中间结果溢出。
	remaining := (math.MaxInt - 12) / 4
	for _, count := range []int{cfg.MaxGoalRounds, cfg.MaxWaves, cfg.MaxReplans} {
		if count > remaining {
			return Config{}, configError("工作流步数上限超出int范围")
		}
		remaining -= count
	}
	if cfg.MaxTasks > math.MaxInt/cfg.MaxTaskAttempts {
		return Config{}, configError("尝试容量超出int范围")
	}

	if strings.TrimSpace(cfg.Version) == "" {
		return Config{}, configError("必须提供Version")
	}
	if len(cfg.Members) == 0 {
		return Config{}, configError("至少需要一个成员")
	}
	ids := make(map[string]struct{}, len(cfg.Members))
	cfg.Members = slices.Clone(cfg.Members)
	for i, member := range cfg.Members {
		if err := validateMemberConfig(member); err != nil {
			return Config{}, err
		}
		if _, found := ids[member.ID]; found {
			return Config{}, configError("成员ID %q重复", member.ID)
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
		return configError("成员ID不能为空")
	}
	if strings.TrimSpace(member.Version) == "" || strings.TrimSpace(member.ManifestDigest) == "" {
		return configError("成员%q必须提供Version和ManifestDigest", member.ID)
	}
	if member.EffectClass != "read_only" && member.EffectClass != "write" {
		return configError("成员%q的EffectClass %q无效", member.ID, member.EffectClass)
	}

	seen := make(map[string]struct{}, len(member.Claims))
	hasWrite := false
	for _, claim := range member.Claims {
		if strings.TrimSpace(claim.Key) == "" {
			return configError("成员%q的资源声明缺少键", member.ID)
		}
		if claim.Mode != "read" && claim.Mode != "write" {
			return configError("成员%q的资源声明模式%q无效", member.ID, claim.Mode)
		}
		if _, found := seen[claim.Key]; found {
			return configError("成员%q的资源声明%q重复", member.ID, claim.Key)
		}
		seen[claim.Key] = struct{}{}
		hasWrite = hasWrite || claim.Mode == "write"
	}
	if hasWrite != (member.EffectClass == "write") {
		return configError("成员%q的资源声明与EffectClass %q不一致", member.ID, member.EffectClass)
	}
	return nil
}

func configError(format string, args ...any) error {
	return agentErrors.New(agentErrors.CodeInvalidConfig, fmt.Sprintf(format, args...)).
		WithComponent("planning").WithOperation("validate_config").WithContext("config_key", "normalize_config")
}
