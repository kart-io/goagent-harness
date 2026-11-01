package preset

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/kart-io/goagent-harness/sandbox"
)

func prepareSandbox(ctx context.Context, a *Assembler, c *WorkspaceConfig) (*sandbox.Session, error) {
	if a.host.Sandboxes == nil {
		return nil, errors.New("executor=sandbox requires Host.Sandboxes")
	}
	parts := append(slices.Clone(a.opts.scope), a.ComponentID())
	scope, err := json.Marshal(parts)
	if err != nil {
		return nil, err
	}
	session, err := a.host.Sandboxes.Open(ctx, sandbox.Request{Provider: c.Provider, Scope: string(scope), Image: c.Image, Network: c.Network})
	if session != nil {
		a.OnClose(func() error {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return session.Close(cleanup)
		})
	}
	if err != nil {
		return nil, err
	}
	info := session.Info()
	a.SetInfo("sandbox_id", info.ID)
	a.SetInfo("sandbox_root", info.Root)
	a.SetInfo("provider", info.Provider)
	a.SetInfo("provider_version", info.Version)
	a.SetInfo("isolated", strconv.FormatBool(info.Isolated))
	a.SetInfo("remote", strconv.FormatBool(info.Capabilities.Remote))
	a.SetInfo("container", strconv.FormatBool(info.Capabilities.Container))
	a.SetInfo("network", strconv.FormatBool(info.Capabilities.Network))
	a.SetInfo("read_only_root", strconv.FormatBool(info.Capabilities.ReadOnlyRoot))
	return session, nil
}
