#!/usr/bin/env bash
# 验证发布基线与本地两个模块；只在临时目录创建 workspace。
set -euo pipefail
harness_root="$(cd "$(dirname "$0")/.." && pwd -P)"
core_root="${1:-$harness_root/../goagent}"
core_root="$(cd "$core_root" && pwd -P)"
if [[ "$(GOWORK=off go -C "$core_root" list -m)" != github.com/kart-io/goagent ]]; then
  echo "指定目录不是 GoAgent 模块" >&2
  exit 1
fi
(cd "$harness_root" && GOWORK=off go test ./... && GOWORK=off go build ./...)
workspace_dir="$(mktemp -d "${TMPDIR:-/tmp}/goagent-harness-verify.XXXXXX")"
trap 'rm -rf "$workspace_dir"' EXIT
(cd "$workspace_dir" && GOWORK=off go work init "$core_root" "$harness_root")
export GOWORK="$workspace_dir/go.work"
(cd "$core_root" && go test ./... && bash scripts/verify_imports.sh)
(cd "$harness_root" && go test ./... && go vet ./...)
