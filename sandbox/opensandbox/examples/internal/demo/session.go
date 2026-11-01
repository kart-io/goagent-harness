// Package demo shares host configuration and bounded cleanup for examples.
package demo

import (
	"context"
	"errors"
	"fmt"
	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/kart-io/goagent-harness/sandbox"
	opensandbox "github.com/kart-io/goagent-harness/sandbox/opensandbox"
	"os"
	"time"
)

func Run(use func(context.Context, *sandbox.Session) error) (err error) {
	domain, image := os.Getenv("HARNESS_SANDBOX_DOMAIN"), os.Getenv("HARNESS_SANDBOX_IMAGE")
	if domain == "" || image == "" {
		return errors.New("HARNESS_SANDBOX_DOMAIN and HARNESS_SANDBOX_IMAGE required")
	}
	p, err := opensandbox.New(opensandbox.Options{ExecdImage: opensandbox.SupportedExecdImage, Connection: sdk.ConnectionConfig{Domain: domain, Protocol: "http", APIKey: os.Getenv("HARNESS_SANDBOX_API_KEY"), UseServerProxy: true}})
	if err != nil {
		return err
	}
	registry := &sandbox.Registry{Namespace: fmt.Sprintf("example-%d", time.Now().UnixNano()), Providers: map[string]sandbox.Provider{"opensandbox": p}}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err = errors.Join(err, registry.Close(ctx))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	session, err := registry.Open(ctx, sandbox.Request{Provider: "opensandbox", Scope: "example", Image: image, Network: true})
	if err != nil {
		return err
	}
	return use(ctx, session)
}
