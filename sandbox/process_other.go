//go:build !unix

package sandbox

import "os/exec"

func isolate(c *exec.Cmd) {}
func kill(c *exec.Cmd) error {
	if c.Process != nil {
		return c.Process.Kill()
	}
	return nil
}
