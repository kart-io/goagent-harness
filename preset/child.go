package preset

import (
	"slices"

	"github.com/kart-io/goagent/tools"
)

// childSettings 在发布后取得完整父权限和沙箱约束。
func (a *Assembler) childSettings(parent *tools.Pipeline, target *Resolved, identity string, toolFilter ToolFilter, template bool) (Host, buildOptions) {
	parentSandbox := Sandbox("")
	var parentNetwork *bool
	if a.opts.parent != nil {
		parentSandbox = a.opts.parent.sandbox
		parentNetwork = a.opts.parent.network
	}
	if a.workspace != nil {
		if parentSandbox == "" || a.workspaceInfo.Sandbox.rank() < parentSandbox.rank() {
			parentSandbox = a.workspaceInfo.Sandbox
		}
		network := a.workspaceInfo.Network
		if parentNetwork != nil {
			network = network && *parentNetwork
		}
		parentNetwork = &network
	}
	filter := toolFilter
	childHost := a.host
	if childHost.AgentID == "" {
		childHost.AgentID = a.preset.ID
	}
	childHost.AgentID += "/" + identity
	policy, ceiling, controller, envAllowlist := a.workspaceInfo.Policy, a.workspaceInfo.Ceiling, a.controller, a.workspaceInfo.EnvAllowlist
	if a.workspace == nil && a.opts.parent != nil {
		policy, ceiling, controller, envAllowlist = a.opts.parent.policy, a.opts.parent.ceiling, a.opts.parent.controller, a.opts.parent.envAllowlist
	}
	scope := append(slices.Clone(a.opts.scope), identity, target.ID)
	options := buildOptions{
		depth: a.Depth() + 1,
		scope: scope,
		parent: &parentScope{network: parentNetwork, pre: parent.Pre, guards: parent.Guards, sandbox: parentSandbox, denyApproval: a.denyApproval,
			policy: policy, ceiling: ceiling, controller: controller, envAllowlist: envAllowlist},
		filter:         &filter,
		template:       a.opts.template || template,
		childSandboxes: a.opts.childSandboxes,
	}
	return childHost, options
}
