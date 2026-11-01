// Package opensandbox adapts the pinned OpenSandbox Docker service to Harness.
// It is an optional module; images require Linux, python3 and sleep.
package opensandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/harness/env"
)

// SupportedExecdImage is a trusted deployment pin, not an image supplied to Create.
// The host must configure its Docker control service with this exact image.
const SupportedExecdImage = "opensandbox/execd@sha256:c75836c46ef4302afccec182d5d7339eb2f905f51e3d73b7aa17bb76c832526c"

const providerVersion = "opensandbox/1.2.0+harness-fs1"

// Options contains trusted host configuration, never model tool arguments.
// The first adapter requires a Docker service and server proxy routing.
// Zero limits select bounded defaults. Custom HTTP transports must honor context.
type Options struct {
	// ExecdImage attests that the trusted Docker service pins this image.
	// Execd Ping has no build version; this cannot be discovered remotely.
	ExecdImage                                                          string
	Connection                                                          sdk.ConnectionConfig
	Root                                                                string
	Python                                                              string
	TTL, CleanupTimeout, OperationTimeout, StartupTimeout, PollInterval time.Duration
	MaxOutputBytes, MaxResponseBytes, MaxFileBytes                      int64
	MaxWalkEntries                                                      int
	ResourceLimits                                                      sdk.ResourceLimits
}
type provider struct {
	options   Options
	lifecycle *sdk.LifecycleClient
	http      *http.Client
	base      *url.URL
}

// New validates configuration before any remote allocation.
func New(o Options) (sandbox.Provider, error) {
	if err := checkVersion(sdk.Version, "SDK"); err != nil {
		return nil, err
	}
	if o.ExecdImage != SupportedExecdImage {
		return nil, fmt.Errorf("%w: trusted Docker deployment must pin SupportedExecdImage", sandbox.ErrUnsupported)
	}
	if !o.Connection.UseServerProxy || o.Connection.Retry != nil {
		return nil, fmt.Errorf("%w: server proxy required; SDK retries disabled", sandbox.ErrUnsupported)
	}
	if o.Root == "" {
		o.Root = "/workspace/goagent"
	}
	if o.Python == "" {
		o.Python = "python3"
	}
	if !path.IsAbs(o.Root) || path.Clean(o.Root) != o.Root || o.Root == "/" || strings.ContainsRune(o.Root, 0) || len(o.Root) > 2048 {
		return nil, errors.New("absolute clean non-root workspace required")
	}
	if strings.ContainsRune(o.Python, 0) {
		return nil, errors.New("invalid python executable")
	}
	defaults := []struct {
		v *time.Duration
		d time.Duration
	}{{&o.TTL, 10 * time.Minute}, {&o.CleanupTimeout, 5 * time.Second}, {&o.OperationTimeout, 30 * time.Second}, {&o.StartupTimeout, 120 * time.Second}, {&o.PollInterval, 50 * time.Millisecond}}
	for _, d := range defaults {
		if *d.v == 0 {
			*d.v = d.d
		}
		if *d.v < 0 {
			return nil, errors.New("negative duration")
		}
	}
	if o.TTL < time.Second || o.TTL > 24*time.Hour {
		return nil, errors.New("TTL must be 1s..24h")
	}
	if o.MaxOutputBytes == 0 {
		o.MaxOutputBytes = 1 << 20
	}
	if o.MaxResponseBytes == 0 {
		o.MaxResponseBytes = 8 << 20
	}
	if o.MaxFileBytes == 0 {
		o.MaxFileBytes = 4 << 20
	}
	if o.MaxWalkEntries == 0 {
		o.MaxWalkEntries = 10000
	}
	if o.MaxFileBytes < 1 || o.MaxFileBytes > 64<<20 || o.MaxResponseBytes < o.MaxFileBytes*4/3+65536 || o.MaxResponseBytes > 128<<20 || o.MaxOutputBytes < 1 || o.MaxOutputBytes > o.MaxResponseBytes || o.MaxWalkEntries < 1 {
		return nil, errors.New("invalid file, response, output or walk limit")
	}
	if o.Connection.RequestTimeout < 0 {
		return nil, errors.New("negative request timeout")
	}
	base, err := url.Parse(o.Connection.GetBaseURL())
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "http" && base.Scheme != "https") || base.Path != "" {
		return nil, errors.New("invalid control plane URL")
	}
	// Resolve environment fallbacks once, so identity/auth cannot change mid-session.
	o.Connection.Domain = base.Host
	o.Connection.Protocol = base.Scheme
	o.Connection.APIKey = o.Connection.GetAPIKey()
	o.Connection.AuthHeader = o.Connection.GetAuthHeader()
	o.Connection.Headers = cloneHeaders(o.Connection.Headers)
	rewrite := map[string]string{}
	for k, v := range o.Connection.EndpointHostRewrite {
		rewrite[k] = v
	}
	o.Connection.EndpointHostRewrite = rewrite
	if o.ResourceLimits == nil {
		o.ResourceLimits = sdk.ResourceLimits{"cpu": "500m", "memory": "256Mi"}
	} else {
		copy := sdk.ResourceLimits{}
		for k, v := range o.ResourceLimits {
			copy[k] = v
		}
		o.ResourceLimits = copy
	}
	client := http.Client{}
	if o.Connection.HTTPClient != nil {
		client = *o.Connection.HTTPClient
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = &responseLimitTransport{base: transport, limit: o.MaxResponseBytes}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	p := &provider{options: o, http: &client, base: base}
	headers := cloneHeaders(o.Connection.Headers)
	stripAuth(headers, o.Connection.GetAuthHeader())
	p.lifecycle = sdk.NewLifecycleClientWithCache(base.String()+"/v1", o.Connection.APIKey, nil, sdk.WithHTTPClient(&client), sdk.WithTimeout(o.Connection.GetRequestTimeout()), sdk.WithAuthHeader(o.Connection.AuthHeader), sdk.WithHeaders(headers))
	return p, nil
}
func cloneHeaders(input map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range input {
		out[k] = v
	}
	return out
}
func stripAuth(h map[string]string, custom string) {
	for k := range h {
		if strings.EqualFold(k, "OPEN-SANDBOX-API-KEY") || strings.EqualFold(k, custom) {
			delete(h, k)
		}
	}
}
func (*provider) Version() string { return providerVersion }
func (p *provider) metadata(r sandbox.Request) map[string]string {
	return map[string]string{"harness.version": "opensandbox-1.2.0-fs1", "harness.root": fingerprint(p.options.Root), "harness.scope": fingerprint(r.Scope), "harness.image": fingerprint(r.Image), "harness.network": "true"}
}
func fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
}
func validateRequest(r sandbox.Request) error {
	if !r.Network {
		return fmt.Errorf("%w: network=false", sandbox.ErrUnsupported)
	}
	if strings.TrimSpace(r.Scope) == "" || strings.TrimSpace(r.Image) == "" || len(r.Scope) > 4096 || len(r.Image) > 2048 {
		return errors.New("scope and image required within limits")
	}
	return nil
}
func token() (string, error) {
	var b [16]byte
	_, e := rand.Read(b[:])
	return hex.EncodeToString(b[:]), e
}
func (p *provider) Create(ctx context.Context, r sandbox.Request) (sandbox.Environment, error) {
	if e := validateRequest(r); e != nil {
		return nil, e
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	owner, e := token()
	if e != nil {
		return nil, e
	}
	instance := &environment{provider: p, owner: owner}
	metadata := p.metadata(r)
	metadata["harness.owner"] = owner
	seconds := int(p.options.TTL / time.Second)
	createCtx, cancel := context.WithTimeout(ctx, p.options.StartupTimeout)
	defer cancel()
	if err := p.checkServer(createCtx); err != nil {
		return nil, err
	}
	info, e := p.lifecycle.CreateSandbox(createCtx, sdk.CreateSandboxRequest{Image: &sdk.ImageSpec{URI: r.Image}, Timeout: &seconds, ResourceLimits: p.options.ResourceLimits, Metadata: metadata, Entrypoint: []string{"sleep", "infinity"}})
	if e != nil {
		var api *sdk.APIError
		if errors.As(e, &api) && api.StatusCode >= 400 && api.StatusCode < 500 && api.StatusCode != 408 {
			return nil, e
		}
		return instance, &sandbox.UnconfirmedError{CommandID: "create:" + owner, Cause: e}
	}
	instance.id = info.ID
	if instance.id == "" {
		return instance, &sandbox.UnconfirmedError{CommandID: "create:" + owner, Cause: errors.New("create response missing ID")}
	}
	e = p.prepare(createCtx, instance, r, true)
	return instance, e
}
func (p *provider) Connect(ctx context.Context, id string, r sandbox.Request) (sandbox.Environment, error) {
	if e := validateRequest(r); e != nil {
		return nil, e
	}
	if id == "" || strings.ContainsAny(id, "/\x00") {
		return nil, errors.New("invalid sandbox ID")
	}
	instance := &environment{provider: p, id: id}
	startup, cancel := context.WithTimeout(ctx, p.options.StartupTimeout)
	defer cancel()
	if err := p.checkServer(startup); err != nil {
		return nil, err
	}
	if e := p.prepare(startup, instance, r, false); e != nil {
		return instance, e
	}
	return instance, nil
}
func (p *provider) prepare(ctx context.Context, e *environment, r sandbox.Request, create bool) error {
	for {
		info, err := p.lifecycle.GetSandbox(ctx, e.id)
		if err != nil {
			return err
		}
		if info.ID != e.id || info.Image == nil || info.Image.URI != r.Image {
			return errors.New("sandbox identity/image mismatch")
		}
		for k, v := range p.metadata(r) {
			if info.Metadata[k] != v {
				return fmt.Errorf("sandbox metadata mismatch: %s", k)
			}
		}
		if info.Status.State == sdk.StateRunning {
			break
		}
		if info.Status.State == sdk.StateFailed || info.Status.State == sdk.StateTerminated {
			return fmt.Errorf("sandbox state %s", info.Status.State)
		}
		if err = wait(ctx, p.options.PollInterval); err != nil {
			return err
		}
	}
	proxy := true
	endpoint, err := p.lifecycle.GetEndpoint(ctx, e.id, sdk.DefaultExecdPort, &proxy)
	if err != nil {
		return err
	}
	raw := p.options.Connection.RewriteEndpointURL(endpoint.Endpoint)
	if !strings.Contains(raw, "://") {
		raw = p.base.Scheme + "://" + raw
	}
	address, err := url.Parse(raw)
	if err != nil || address.User != nil || address.Scheme != p.base.Scheme || !strings.EqualFold(address.Host, p.base.Host) || address.RawQuery != "" || address.Fragment != "" {
		return errors.New("proxy endpoint must use control plane origin")
	}
	headers := cloneHeaders(p.options.Connection.Headers)
	for k, v := range endpoint.Headers {
		headers[k] = v
	}
	stripAuth(headers, p.options.Connection.AuthHeader)
	headers["OPEN-SANDBOX-API-KEY"] = p.options.Connection.APIKey
	client := sdk.NewExecdClient(address.String(), "", sdk.WithHTTPClient(p.http), sdk.WithTimeout(p.options.Connection.GetRequestTimeout()), sdk.WithHeaders(headers))
	// Running lifecycle state can precede the execd listener. Only retry the
	// read-only readiness probe; never replay creation or a business command.
	for {
		if err = client.Ping(ctx); err == nil {
			break
		}
		if err = wait(ctx, p.options.PollInterval); err != nil {
			return err
		}
	}
	e.runner = &commandRunner{sandboxID: e.id, client: client, poll: p.options.PollInterval, cleanupTimeout: p.options.CleanupTimeout, requestTimeout: p.options.Connection.GetRequestTimeout()}
	e.files = newFileSystem(p.options.Root, p.options.MaxFileBytes, p.options.MaxWalkEntries, e.fileCall)
	return e.files.initialize(ctx, create)
}

type environment struct {
	provider  *provider
	id, owner string
	runner    *commandRunner
	files     *fileSystem
	mu        sync.Mutex
	closed    bool
	tainted   error
}

func (e *environment) ID() string     { return e.id }
func (e *environment) Root() string   { return e.provider.options.Root }
func (e *environment) Isolated() bool { return true }
func (e *environment) FileSystem() env.FileSystem {
	if e.files == nil {
		return nil
	}
	return e.files
}
func (e *environment) Executor() env.Executor { return e }
func (e *environment) Capabilities() env.Capabilities {
	return env.Capabilities{Remote: true, Container: true, Network: true}
}
func (e *environment) check() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return sandbox.ErrClosed
	}
	return e.tainted
}
func (e *environment) remember(err error) {
	if errors.Is(err, sandbox.ErrUnconfirmed) {
		e.mu.Lock()
		e.tainted = err
		e.mu.Unlock()
	}
}
func (e *environment) Run(ctx context.Context, c env.Command) (env.Result, error) {
	if err := e.check(); err != nil {
		return env.Result{}, err
	}
	if e.files == nil || e.runner == nil {
		return env.Result{}, errors.New("sandbox not initialized")
	}
	if path.IsAbs(c.Dir) {
		return env.Result{}, env.ErrOutsideWorkspace
	}
	op, cancel := context.WithTimeout(ctx, e.provider.options.OperationTimeout)
	defer cancel()
	dir, err := e.files.ResolveContext(op, c.Dir)
	if err != nil {
		return env.Result{}, err
	}
	result, err := e.runner.run(op, c.Argv, path.Join(e.Root(), dir), e.provider.options.MaxOutputBytes)
	e.remember(err)
	return result, err
}
func (e *environment) fileCall(ctx context.Context, q fileRequest) (output []byte, err error) {
	if err = e.check(); err != nil {
		return nil, err
	}
	op, cancel := context.WithTimeout(ctx, e.provider.options.OperationTimeout)
	defer cancel()
	data, err := json.Marshal(q)
	if err != nil {
		return nil, err
	}
	id, err := token()
	if err != nil {
		return nil, err
	}
	remote := "/tmp/.harness-request-" + id + ".json"
	// Payload is uploaded rather than put in argv: Linux limits each argument.
	defer func() {
		clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.provider.options.CleanupTimeout)
		defer cancel()
		cleanup := e.runner.client.DeleteFiles(clean, []string{remote})
		err = errors.Join(err, cleanup)
		e.remember(err)
	}()
	if err = e.runner.client.UploadFile(op, bytes.NewReader(data), sdk.UploadFileOptions{FileName: "request.json", Metadata: sdk.FileMetadata{Path: remote, Mode: sdk.OctalMode(0600)}}); err != nil {
		return nil, err
	}
	result, err := e.runner.run(op, []string{e.provider.options.Python, "-c", filesystemHelper, e.Root(), remote, fmt.Sprint(e.provider.options.MaxFileBytes), fmt.Sprint(e.provider.options.MaxWalkEntries)}, "/tmp", e.provider.options.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("filesystem helper exit %d: %s", result.ExitCode, string(result.Output))
	}
	return result.Output, nil
}
func (e *environment) Close(context.Context) error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	return nil
}
func notFound(err error) bool {
	var api *sdk.APIError
	return errors.As(err, &api) && api.StatusCode == 404
}
func (e *environment) delete(ctx context.Context, id string) error {
	if err := e.provider.lifecycle.DeleteSandbox(ctx, id); err != nil && !notFound(err) {
		return err
	}
	for {
		info, err := e.provider.lifecycle.GetSandbox(ctx, id)
		if notFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Status.State == sdk.StateTerminated {
			return nil
		}
		if err = wait(ctx, e.provider.options.PollInterval); err != nil {
			return err
		}
	}
}
func (e *environment) Destroy(ctx context.Context) error {
	if e.id != "" {
		return e.delete(ctx, e.id)
	}
	// A timed-out create can still allocate later. Keep cleanup retryable until
	// an owner-token match is observed, never interpret an empty list as success.
	for page := 1; ; page++ {
		list, err := e.provider.lifecycle.ListSandboxes(ctx, sdk.ListOptions{Metadata: map[string]string{"harness.owner": e.owner}, Page: page, PageSize: 100})
		if err != nil {
			return err
		}
		var failures error
		found := false
		for _, item := range list.Items {
			if item.Metadata["harness.owner"] == e.owner {
				found = true
				failures = errors.Join(failures, e.delete(ctx, item.ID))
			}
		}
		if failures != nil {
			return failures
		}
		if found {
			return nil
		}
		if !list.Pagination.HasNextPage {
			break
		}
	}
	return &sandbox.UnconfirmedError{CommandID: "create:" + e.owner, Cause: errors.New("creation not reconciled; retain token and retry cleanup before TTL")}
}

func checkVersion(version, component string) error {
	if version != "1.2.0" {
		return fmt.Errorf("%w: %s release %q; require 1.2.0", sandbox.ErrUnsupported, component, version)
	}
	return nil
}
func (p *provider) checkServer(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, p.options.Connection.GetRequestTimeout())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base.String()+"/version", nil)
	if err != nil {
		return err
	}
	for k, v := range p.options.Connection.Headers {
		request.Header.Set(k, v)
	}
	request.Header.Del("OPEN-SANDBOX-API-KEY")
	request.Header.Del(p.options.Connection.AuthHeader)
	request.Header.Set(p.options.Connection.AuthHeader, p.options.Connection.APIKey)
	response, err := p.http.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: server version endpoint HTTP %d", sandbox.ErrUnsupported, response.StatusCode)
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	var version struct {
		Version string `json:"version"`
	}
	if err = json.Unmarshal(raw, &version); err != nil {
		return err
	}
	return checkVersion(version.Version, "server")
}
