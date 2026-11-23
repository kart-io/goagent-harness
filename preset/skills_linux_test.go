//go:build linux

package preset

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSkillsSourceFIFORejectedWithoutWriter(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".agents"), 0700))
	fifo := filepath.Join(root, ".agents", "skills")
	require.NoError(t, syscall.Mkfifo(fifo, 0600))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p := mustGet(t, trusted(`presets: [{id: p, components: [{id: s, use: skills}]}]`))
	artifact := t.TempDir()
	done := make(chan error, 1)
	go func() {
		asm, err := Build(ctx, p, Host{Root: root, ArtifactDir: artifact})
		if asm != nil {
			_ = asm.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-ctx.Done():
		writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = writer.Close()
		}
		<-done
		t.Fatal("Build blocked opening a FIFO skill source")
	}
	// The same acquisition rule applies to explicit and home-relative sources.
	t.Setenv("HOME", root)
	for _, source := range []string{fifo, "~/.agents/skills"} {
		_, err := openSkillDirectory(root, source)
		require.Error(t, err, source)
	}
}
