package opensandbox

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/harness/env"
	"io/fs"
	"path"
	"strings"
	"time"
)

//go:embed filesystem.py
var filesystemHelper string
var (
	ErrFileLimit = errors.New("sandbox file size limit exceeded")
	ErrWalkLimit = errors.New("sandbox walk entry limit exceeded")
)

type fileRequest struct {
	ProtectedNames []string    `json:"protected_names,omitempty"`
	Op             string      `json:"op"`
	Path           []byte      `json:"path,omitempty"`
	Data           []byte      `json:"data,omitempty"`
	Version        env.Version `json:"version,omitempty"`
	CreateIfAbsent bool        `json:"create_if_absent,omitempty"`
	Create         bool        `json:"create,omitempty"`
	Perm           uint32      `json:"perm,omitempty"`
	After          []byte      `json:"after,omitempty"`
	Limit          int         `json:"limit,omitempty"`
}
type wireInfo struct {
	Path  []byte `json:"path"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	Kind  string `json:"kind"`
	Mtime int64  `json:"mtime"`
}
type fileReply struct {
	Protocol  int         `json:"protocol"`
	ErrorCode string      `json:"error_code"`
	Message   string      `json:"message"`
	Path      []byte      `json:"path"`
	Data      []byte      `json:"data"`
	Version   env.Version `json:"version"`
	Info      wireInfo    `json:"info"`
	Entries   []wireInfo  `json:"entries"`
	More      bool        `json:"more"`
}
type fileSystem struct {
	protectedNames []string
	root           string
	maxFile        int64
	maxEntries     int
	call           func(context.Context, fileRequest) ([]byte, error)
}

func newFileSystem(root string, maxFile int64, maxEntries int, call func(context.Context, fileRequest) ([]byte, error)) *fileSystem {
	return &fileSystem{root: root, maxFile: maxFile, maxEntries: maxEntries, call: call}
}
func (f *fileSystem) request(ctx context.Context, q fileRequest) (fileReply, error) {
	var r fileReply
	if err := ctx.Err(); err != nil {
		return r, err
	}
	q.ProtectedNames = append([]string(nil), f.protectedNames...)
	raw, err := f.call(ctx, q)
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("filesystem response: %w", err)
	}
	if r.Protocol != 1 {
		return r, errors.New("filesystem protocol mismatch")
	}
	var cause error
	switch r.ErrorCode {
	case "":
		return r, nil
	case "outside":
		cause = env.ErrOutsideWorkspace
	case "exists":
		cause = env.ErrExists
	case "stale":
		cause = env.ErrStale
	case "not_exist":
		cause = fs.ErrNotExist
	case "permission":
		cause = fs.ErrPermission
	case "invalid":
		cause = fs.ErrInvalid
	case "file_limit":
		cause = ErrFileLimit
	case "unconfirmed":
		cause = sandbox.ErrUnconfirmed
	default:
		cause = fmt.Errorf("remote filesystem %s: %s", r.ErrorCode, r.Message)
	}
	return r, &fs.PathError{Op: q.Op, Path: string(q.Path), Err: cause}
}
func (f *fileSystem) initialize(ctx context.Context, create bool) error {
	_, err := f.request(ctx, fileRequest{Op: "init", Create: create})
	return err
}
func validRelative(p string) bool {
	return p != "" && !strings.ContainsRune(p, 0) && !path.IsAbs(p) && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") && len(p) <= 4096
}
func (f *fileSystem) Resolve(p string) (string, error) {
	return f.ResolveContext(context.Background(), p)
}
func (f *fileSystem) ResolveContext(ctx context.Context, p string) (string, error) {
	r, e := f.request(ctx, fileRequest{Op: "resolve", Path: []byte(p)})
	if e != nil {
		return "", e
	}
	if !validRelative(string(r.Path)) {
		return "", errors.New("invalid resolved path")
	}
	return string(r.Path), nil
}
func (f *fileSystem) ReadFile(ctx context.Context, p string) ([]byte, env.Version, error) {
	r, e := f.request(ctx, fileRequest{Op: "read", Path: []byte(p)})
	return r.Data, r.Version, e
}
func (f *fileSystem) WriteFile(ctx context.Context, p string, b []byte, i env.WriteIntent) (env.Version, error) {
	if int64(len(b)) > f.maxFile {
		return "", ErrFileLimit
	}
	r, e := f.request(ctx, fileRequest{Op: "write", Path: []byte(p), Data: b, Version: i.IfVersion, CreateIfAbsent: i.CreateIfAbsent, Perm: uint32(i.Perm.Perm())})
	return r.Version, e
}
func (f *fileSystem) Remove(ctx context.Context, p string) error {
	_, e := f.request(ctx, fileRequest{Op: "remove", Path: []byte(p)})
	return e
}
func convertInfo(w wireInfo) (env.Info, error) {
	p := string(w.Path)
	if !validRelative(p) {
		return env.Info{}, errors.New("invalid metadata path")
	}
	m := fs.FileMode(w.Mode & 0777)
	if w.Mode&04000 != 0 {
		m |= fs.ModeSetuid
	}
	if w.Mode&02000 != 0 {
		m |= fs.ModeSetgid
	}
	if w.Mode&01000 != 0 {
		m |= fs.ModeSticky
	}
	switch w.Kind {
	case "dir":
		m |= fs.ModeDir
	case "symlink":
		m |= fs.ModeSymlink
	case "fifo":
		m |= fs.ModeNamedPipe
	case "socket":
		m |= fs.ModeSocket
	case "block":
		m |= fs.ModeDevice
	case "char":
		m |= fs.ModeDevice | fs.ModeCharDevice
	case "regular":
	default:
		return env.Info{}, errors.New("invalid file kind")
	}
	return env.Info{Path: p, Size: w.Size, Mode: m, ModTime: time.Unix(0, w.Mtime)}, nil
}
func (f *fileSystem) Stat(ctx context.Context, p string) (env.Info, error) {
	r, e := f.request(ctx, fileRequest{Op: "stat", Path: []byte(p)})
	if e != nil {
		return env.Info{}, e
	}
	return convertInfo(r.Info)
}
func (f *fileSystem) Walk(ctx context.Context, p string, fn func(env.Info) error) error {
	if fn == nil {
		return fs.ErrInvalid
	}
	r, e := f.request(ctx, fileRequest{Op: "entry", Path: []byte(p)})
	if e != nil {
		return e
	}
	first, e := convertInfo(r.Info)
	if e != nil {
		return e
	}
	visited := 0
	var visit func(env.Info) error
	visit = func(i env.Info) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if visited >= f.maxEntries {
			return ErrWalkLimit
		}
		visited++
		e := fn(i)
		if e != nil {
			if errors.Is(e, fs.SkipDir) && i.IsDir() {
				return nil
			}
			return e
		}
		if !i.IsDir() {
			return nil
		}
		var after []byte
		for {
			page, e := f.request(ctx, fileRequest{Op: "list", Path: []byte(i.Path), After: after, Limit: 64})
			if e != nil {
				return e
			}
			for _, w := range page.Entries {
				child, e := convertInfo(w)
				if e != nil {
					return e
				}
				if path.Dir(child.Path) != i.Path || child.Path == i.Path || path.Base(child.Path) <= string(after) {
					return errors.New("invalid directory page")
				}
				after = []byte(path.Base(child.Path))
				if e = visit(child); e != nil {
					if errors.Is(e, fs.SkipDir) {
						return nil
					}
					return e
				}
			}
			if !page.More {
				return nil
			}
			if len(page.Entries) == 0 {
				return errors.New("empty directory continuation")
			}
		}
	}
	e = visit(first)
	if errors.Is(e, fs.SkipAll) || errors.Is(e, fs.SkipDir) {
		return nil
	}
	return e
}

func (f *fileSystem) WithProtectedNames(names ...string) env.FileSystem {
	copy := *f
	copy.protectedNames = append(append([]string(nil), f.protectedNames...), names...)
	return &copy
}
