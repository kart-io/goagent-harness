package sshsandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/harness/env"
)

// fileSystem 用远端命令实现文件操作。这些命令不进沙箱，路径边界在这里检查：每次操作都解析真实路径，符号链接逃不出工作区。
// 检查和操作之间的竞争仍然存在，运行中的命令可能在间隙里替换路径。
type fileSystem struct {
	client    *client
	root      string
	lock      string
	protected []string
}

// WithProtectedNames 实现 sandbox.MutationProtector。
func (f *fileSystem) WithProtectedNames(names ...string) env.FileSystem {
	guarded := *f
	guarded.protected = append(slices.Clone(f.protected), names...)
	return &guarded
}

func (f *fileSystem) Resolve(p string) (string, error) {
	return f.ResolveContext(context.Background(), p)
}

// 目标还不存在时解析最深的已存在祖先，否则 a/link/new 会绕过 a/link 指向外部的检查。
const resolveScript = `p=$1
while [ ! -e "$p" ] && [ ! -L "$p" ]; do p=$(dirname "$p"); done
readlink -f "$p"
`

// ResolveContext 实现 sandbox.ContextResolver。
func (f *fileSystem) ResolveContext(ctx context.Context, p string) (string, error) {
	if p == f.root {
		p = "."
	} else if strings.HasPrefix(p, f.root+"/") {
		p = strings.TrimPrefix(p, f.root+"/")
	}
	clean := path.Clean(p)
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", env.ErrOutsideWorkspace
	}
	stdout, stderr, code, err := f.client.run(ctx, joinShell("sh", "-c", resolveScript, "sh", path.Join(f.root, clean)), nil, false)
	if err != nil {
		return "", err
	}
	real := strings.TrimSuffix(string(stdout), "\n")
	// 链接的目标解析不了时没有输出，同样不能当作在工作区内。
	if code != 0 {
		return "", fmt.Errorf("%w: %s", env.ErrOutsideWorkspace, stderr)
	}
	if real != f.root && !strings.HasPrefix(real, f.root+"/") {
		return "", env.ErrOutsideWorkspace
	}
	return clean, nil
}

func (f *fileSystem) abs(ctx context.Context, p string) (string, error) {
	clean, err := f.ResolveContext(ctx, p)
	if err != nil {
		return "", err
	}
	return path.Join(f.root, clean), nil
}

func (f *fileSystem) mutable(ctx context.Context, p string) (string, error) {
	clean, err := f.ResolveContext(ctx, p)
	if err != nil {
		return "", err
	}
	for _, part := range strings.Split(clean, "/") {
		if slices.Contains(f.protected, part) {
			return "", fmt.Errorf("%w: %s 受保护", sandbox.ErrDenied, clean)
		}
	}
	return path.Join(f.root, clean), nil
}

func notFound(stderr []byte) bool { return bytes.Contains(stderr, []byte("No such file")) }

func (f *fileSystem) info(rel string, size, rawMode string, modified string) (env.Info, error) {
	bytesize, err := strconv.ParseInt(size, 10, 64)
	if err != nil {
		return env.Info{}, err
	}
	raw, err := strconv.ParseUint(rawMode, 16, 32)
	if err != nil {
		return env.Info{}, err
	}
	seconds, err := strconv.ParseInt(modified, 10, 64)
	if err != nil {
		return env.Info{}, err
	}
	mode := fs.FileMode(raw & 0o777)
	switch raw & 0o170000 {
	case 0o040000:
		mode |= fs.ModeDir
	case 0o120000:
		mode |= fs.ModeSymlink
	}
	return env.Info{Path: rel, Size: bytesize, Mode: mode, ModTime: time.Unix(seconds, 0)}, nil
}

// 记录以 NUL 分隔，文件名可以包含换行，也不能伪造别的条目。
const statFormat = `%s|%f|%Y|%n\0`

func (f *fileSystem) parse(record string) (env.Info, error) {
	fields := strings.SplitN(record, "|", 4)
	if len(fields) != 4 {
		return env.Info{}, fmt.Errorf("无效的 stat 输出: %q", record)
	}
	name := fields[3]
	rel := "."
	if name != f.root {
		if !strings.HasPrefix(name, f.root+"/") {
			return env.Info{}, env.ErrOutsideWorkspace
		}
		rel = strings.TrimPrefix(name, f.root+"/")
	}
	return f.info(rel, fields[0], fields[1], fields[2])
}

func (f *fileSystem) Stat(ctx context.Context, p string) (env.Info, error) {
	target, err := f.abs(ctx, p)
	if err != nil {
		return env.Info{}, err
	}
	stdout, stderr, code, err := f.client.run(ctx, joinShell("stat", "-L", "--printf="+statFormat, target), nil, false)
	if err != nil {
		return env.Info{}, err
	}
	if code != 0 {
		if notFound(stderr) {
			return env.Info{}, fs.ErrNotExist
		}
		return env.Info{}, fmt.Errorf("stat: %s", stderr)
	}
	return f.parse(strings.TrimSuffix(string(stdout), "\x00"))
}

func versionOf(data []byte) env.Version {
	sum := sha256.Sum256(data)
	return env.Version(hex.EncodeToString(sum[:]))
}

func (f *fileSystem) ReadFile(ctx context.Context, p string) ([]byte, env.Version, error) {
	target, err := f.abs(ctx, p)
	if err != nil {
		return nil, "", err
	}
	stdout, stderr, code, err := f.client.run(ctx, joinShell("cat", target), nil, false)
	if err != nil {
		return nil, "", err
	}
	if code != 0 {
		if notFound(stderr) {
			return nil, "", fs.ErrNotExist
		}
		return nil, "", fmt.Errorf("read: %s", stderr)
	}
	return stdout, versionOf(stdout), nil
}

const writeScript = `set -e
mkdir -p "$(dirname "$1")"
tmp=$(mktemp "$1.XXXXXX")
trap 'rm -f "$tmp"' EXIT
cat > "$tmp"
exec 9>"$5"
flock 9
if [ "$2" = 1 ] && [ -e "$1" ]; then exit 17; fi
if [ -n "$3" ]; then
  current=$(sha256sum "$1" 2>/dev/null | cut -d' ' -f1)
  if [ "$current" != "$3" ]; then exit 18; fi
fi
if [ -n "$4" ]; then chmod "$4" "$tmp"; elif [ -e "$1" ]; then chmod "$(stat -c %a "$1")" "$tmp"; else chmod 644 "$tmp"; fi
mv "$tmp" "$1"
trap - EXIT
`

func (f *fileSystem) WriteFile(ctx context.Context, p string, data []byte, intent env.WriteIntent) (env.Version, error) {
	target, err := f.mutable(ctx, p)
	if err != nil {
		return "", err
	}
	create, perm := "0", ""
	if intent.CreateIfAbsent {
		create = "1"
	}
	if intent.Perm != 0 {
		perm = strconv.FormatUint(uint64(intent.Perm.Perm()), 8)
	}
	_, stderr, code, err := f.client.run(ctx, joinShell("sh", "-c", writeScript, "write", target, create, string(intent.IfVersion), perm, f.lock), data, false)
	if err != nil {
		return "", err
	}
	switch code {
	case 0:
		return versionOf(data), nil
	case 17:
		return "", env.ErrExists
	case 18:
		return "", env.ErrStale
	}
	return "", fmt.Errorf("write: %s", stderr)
}

func (f *fileSystem) Remove(ctx context.Context, p string) error {
	target, err := f.mutable(ctx, p)
	if err != nil {
		return err
	}
	if target == f.root {
		return fmt.Errorf("%w: 不能删除工作区根目录", sandbox.ErrDenied)
	}
	_, stderr, code, err := f.client.run(ctx, joinShell("rm", "-r", target), nil, false)
	if err != nil {
		return err
	}
	if code != 0 {
		if notFound(stderr) {
			return fs.ErrNotExist
		}
		return fmt.Errorf("remove: %s", stderr)
	}
	return nil
}

func (f *fileSystem) Walk(ctx context.Context, p string, fn func(env.Info) error) error {
	target, err := f.abs(ctx, p)
	if err != nil {
		return err
	}
	stdout, stderr, code, err := f.client.run(ctx, joinShell("find", target, "-exec", "stat", "--printf="+statFormat, "{}", "+"), nil, false)
	if err != nil {
		return err
	}
	if code != 0 {
		if notFound(stderr) && len(stdout) == 0 {
			return fs.ErrNotExist
		}
		return fmt.Errorf("walk: %s", stderr)
	}
	var infos []env.Info
	for _, record := range strings.Split(strings.TrimSuffix(string(stdout), "\x00"), "\x00") {
		info, err := f.parse(record)
		if err != nil {
			return err
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Path < infos[j].Path })
	skipped := ""
	for _, info := range infos {
		if skipped != "" && strings.HasPrefix(info.Path, skipped+"/") {
			continue
		}
		switch err := fn(info); {
		case errors.Is(err, fs.SkipAll):
			return nil
		case errors.Is(err, fs.SkipDir):
			skipped = info.Path
		case err != nil:
			return err
		}
	}
	return nil
}
