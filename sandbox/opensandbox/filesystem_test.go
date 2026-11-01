package opensandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kart-io/goagent/harness/env"
	"github.com/stretchr/testify/require"
)

func localFilesystem(t *testing.T, maxFile int64, maxEntries int) (*fileSystem, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	call := func(ctx context.Context, request fileRequest) ([]byte, error) {
		data, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		temp, err := os.CreateTemp("", "harness-fs-request-")
		if err != nil {
			return nil, err
		}
		defer os.Remove(temp.Name())
		if _, err = temp.Write(data); err != nil {
			temp.Close()
			return nil, err
		}
		if err = temp.Close(); err != nil {
			return nil, err
		}
		output, err := exec.CommandContext(ctx, "python3", "-c", filesystemHelper, root, temp.Name(), fmt.Sprint(maxFile), fmt.Sprint(maxEntries)).CombinedOutput()
		if err != nil {
			return output, fmt.Errorf("helper: %w: %s", err, output)
		}
		return output, nil
	}
	f := newFileSystem(root, maxFile, maxEntries, call)
	require.NoError(t, f.initialize(context.Background(), true))
	return f, root
}
func TestFilesystemBinaryVersionsAndConditionalWrites(t *testing.T) {
	f, _ := localFilesystem(t, 1024, 100)
	ctx := context.Background()
	initial := []byte{0, 1, 2, 255, 127}
	v, err := f.WriteFile(ctx, "folder/文件.bin", initial, env.WriteIntent{CreateIfAbsent: true})
	require.NoError(t, err)
	data, readVersion, err := f.ReadFile(ctx, "folder/文件.bin")
	require.NoError(t, err)
	require.Equal(t, initial, data)
	require.Equal(t, v, readVersion)
	require.Len(t, string(v), 64)
	_, err = f.WriteFile(ctx, "folder/文件.bin", nil, env.WriteIntent{CreateIfAbsent: true})
	require.ErrorIs(t, err, env.ErrExists)
	next, err := f.WriteFile(ctx, "folder/文件.bin", []byte("next"), env.WriteIntent{IfVersion: v})
	require.NoError(t, err)
	require.NotEqual(t, v, next)
	_, err = f.WriteFile(ctx, "folder/文件.bin", nil, env.WriteIntent{IfVersion: v})
	require.ErrorIs(t, err, env.ErrStale)
	info, err := f.Stat(ctx, "folder")
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.NoError(t, f.Remove(ctx, "folder/文件.bin"))
	_, _, err = f.ReadFile(ctx, "folder/文件.bin")
	require.ErrorIs(t, err, fs.ErrNotExist)
}
func TestFilesystemLinksStayWithinRootAndRemoveLinkItself(t *testing.T) {
	f, root := localFilesystem(t, 1024, 100)
	ctx := context.Background()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))
	_, err := f.Resolve("escape/secret")
	require.ErrorIs(t, err, env.ErrOutsideWorkspace)
	_, err = f.WriteFile(ctx, "escape/secret", []byte("changed"), env.WriteIntent{})
	require.ErrorIs(t, err, env.ErrOutsideWorkspace)
	_, err = f.Resolve("../secret")
	require.ErrorIs(t, err, env.ErrOutsideWorkspace)
	_, err = f.WriteFile(ctx, "target", []byte("inside"), env.WriteIntent{})
	require.NoError(t, err)
	require.NoError(t, os.Symlink("target", filepath.Join(root, "alias")))
	resolved, err := f.Resolve("alias")
	require.NoError(t, err)
	require.Equal(t, "target", resolved)
	require.NoError(t, f.Remove(ctx, "alias"))
	data, _, err := f.ReadFile(ctx, "target")
	require.NoError(t, err)
	require.Equal(t, "inside", string(data))
	var links []string
	require.NoError(t, f.Walk(ctx, ".", func(info env.Info) error {
		if info.Mode&fs.ModeSymlink != 0 {
			links = append(links, info.Path)
		}
		return nil
	}))
	require.Contains(t, links, "escape")
}
func TestFilesystemCASCoordinatesDifferentHelperProcesses(t *testing.T) {
	f, _ := localFilesystem(t, 1024, 100)
	ctx := context.Background()
	version, err := f.WriteFile(ctx, "counter", []byte("old"), env.WriteIntent{})
	require.NoError(t, err)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, value := range []string{"first", "second"} {
		wg.Add(1)
		go func(value string) {
			defer wg.Done()
			_, err := f.WriteFile(ctx, "counter", []byte(value), env.WriteIntent{IfVersion: version})
			results <- err
		}(value)
	}
	wg.Wait()
	close(results)
	success, stale := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, env.ErrStale) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, stale)
}
func TestFilesystemWalkSkipAndLimits(t *testing.T) {
	f, root := localFilesystem(t, 8, 2)
	ctx := context.Background()
	for _, name := range []string{"a", "b", "c"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("text"), 0600))
	}
	count := 0
	require.NoError(t, f.Walk(ctx, ".", func(env.Info) error { count++; return fs.SkipAll }))
	require.Equal(t, 1, count)
	count = 0
	err := f.Walk(ctx, ".", func(env.Info) error { count++; return nil })
	require.ErrorIs(t, err, ErrWalkLimit)
	require.Equal(t, 2, count)
	_, err = f.WriteFile(ctx, "large", make([]byte, 9), env.WriteIntent{})
	require.ErrorIs(t, err, ErrFileLimit)
	require.NoError(t, os.WriteFile(filepath.Join(root, "large"), make([]byte, 9), 0600))
	_, _, err = f.ReadFile(ctx, "large")
	require.ErrorIs(t, err, ErrFileLimit)
	f.maxEntries = 100
	var seen []string
	require.NoError(t, f.Walk(ctx, ".", func(i env.Info) error {
		seen = append(seen, i.Path)
		if i.Path == "a" {
			return fs.SkipDir
		}
		return nil
	}))
	require.Equal(t, []string{".", "a"}, seen)
	require.NoError(t, os.Mkdir(filepath.Join(root, "dir"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "dir", "hidden"), nil, 0600))
	seen = nil
	require.NoError(t, f.Walk(ctx, ".", func(i env.Info) error {
		seen = append(seen, i.Path)
		if i.Path == "dir" {
			return fs.SkipDir
		}
		return nil
	}))
	require.NotContains(t, seen, "dir/hidden")
}

func TestResolveAllowsNewNestedFileWithoutCreatingDirectories(t *testing.T) {
	f, root := localFilesystem(t, 1024, 100)
	relative, err := f.Resolve("new/nested/file")
	require.NoError(t, err)
	require.Equal(t, "new/nested/file", relative)
	_, err = os.Stat(filepath.Join(root, "new"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}
