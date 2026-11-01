//go:build !linux

package sandbox

import "os"

func socketFilter(Policy) (*os.File, error) { return nil, ErrUnavailable }
