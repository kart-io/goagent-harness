package coding

import (
	"context"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/harness/env"
	"io/fs"
	"path"
)

// protectedFS sits beneath Observed/Checkpointed: their repeated Resolve calls
// must not turn an authorized filename into a protected path at commit time.
// Cooperating backends additionally enforce excluded names inside mutation.
type protectedFS struct{ env.FileSystem }

func (f protectedFS) writable(ctx context.Context, p string) (string, error) {
	var rel string
	var err error
	if resolver, ok := f.FileSystem.(sandbox.ContextResolver); ok {
		rel, err = resolver.ResolveContext(ctx, p)
	} else {
		rel, err = f.Resolve(p)
	}
	if err != nil {
		return "", err
	}
	if inGitDir(rel) {
		return "", &fs.PathError{Op: "mutate", Path: rel, Err: fs.ErrPermission}
	}
	return rel, nil
}
func (f protectedFS) WriteFile(ctx context.Context, p string, data []byte, intent env.WriteIntent) (env.Version, error) {
	rel, err := f.writable(ctx, p)
	if err != nil {
		return "", err
	}
	return f.FileSystem.WriteFile(ctx, rel, data, intent)
}
func (f protectedFS) Remove(ctx context.Context, p string) error {
	// Resolve only the parent so unlink still removes the entry, not its target.
	parent, err := f.writable(ctx, path.Dir(p))
	if err != nil {
		return err
	}
	if inGitDir(path.Join(parent, path.Base(p))) {
		return &fs.PathError{Op: "remove", Path: p, Err: fs.ErrPermission}
	}
	return f.FileSystem.Remove(ctx, p)
}
