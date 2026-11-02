//go:build !linux

package sandbox

import "os/exec"

const (
	landlockMinABI  = 3
	landlockFullABI = 6
)

func landlockABI() int { return 0 }

func startLandlock(*exec.Cmd, Policy, string) error { return ErrUnavailable }
