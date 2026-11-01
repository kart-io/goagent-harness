//go:build linux

package sandbox

import (
	"encoding/binary"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"runtime"
)

// socketFilter restricts creation of host-facing sockets after bwrap setup.
func socketFilter(p Policy) (*os.File, error) {
	if p.Mode == DangerFullAccess && p.Network == NetworkInherit {
		return nil, nil
	}
	var arch, nr, pair uint32
	switch runtime.GOARCH {
	case "amd64":
		arch = 0xc000003e
		nr = 41
		pair = 53
	case "arm64":
		arch = 0xc00000b7
		nr = 198
		pair = 199
	default:
		return nil, fmt.Errorf("%w: unverified seccomp architecture", ErrUnavailable)
	}
	const (
		ld    = 0x20
		jeq   = 0x15
		jset  = 0x45
		ret   = 0x06
		allow = 0x7fff0000
		kill  = 0x80000000
		deny  = 0x50001
	)
	filters := []unix.SockFilter{{Code: ld, K: 4}, {Code: jeq, K: arch, Jt: 1}, {Code: ret, K: kill}, {Code: ld, K: 0}}
	if runtime.GOARCH == "amd64" {
		filters = append(filters, unix.SockFilter{Code: jset, K: 0x40000000, Jf: 1}, unix.SockFilter{Code: ret, K: kill})
	}
	filters = append(filters, unix.SockFilter{Code: jeq, K: pair, Jf: 1}, unix.SockFilter{Code: ret, K: deny})
	// io_uring can create sockets without executing the socket syscall.
	filters = append(filters, unix.SockFilter{Code: jeq, K: 425, Jf: 1}, unix.SockFilter{Code: ret, K: deny})
	if p.Network == NetworkDeny {
		filters = append(filters, unix.SockFilter{Code: jeq, K: nr, Jf: 1}, unix.SockFilter{Code: ret, K: deny}, unix.SockFilter{Code: ret, K: allow})
	} else {
		filters = append(filters, unix.SockFilter{Code: jeq, K: nr, Jt: 1}, unix.SockFilter{Code: ret, K: allow}, unix.SockFilter{Code: ld, K: 16}, unix.SockFilter{Code: jeq, K: unix.AF_UNIX, Jf: 1}, unix.SockFilter{Code: ret, K: deny}, unix.SockFilter{Code: ret, K: allow})
	}
	f, err := os.CreateTemp("", "harness-seccomp-")
	if err != nil {
		return nil, err
	}
	os.Remove(f.Name())
	for _, filter := range filters {
		if err := binary.Write(f, binary.LittleEndian, filter); err != nil {
			f.Close()
			return nil, err
		}
	}
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
