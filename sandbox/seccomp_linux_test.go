//go:build linux

package sandbox

import (
	"encoding/binary"
	"golang.org/x/sys/unix"
	"io"
	"runtime"
	"testing"
)

func TestSocketFilter(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("architecture unavailable")
	}
	arch, nr := uint32(0xc000003e), uint32(41)
	if runtime.GOARCH == "arm64" {
		arch = 0xc00000b7
		nr = 198
	}
	for _, network := range []Network{NetworkDeny, NetworkInherit} {
		f, err := socketFilter(Policy{Mode: WorkspaceWrite, Network: network})
		if err != nil {
			t.Fatal(err)
		}
		var filters []unix.SockFilter
		for {
			var filter unix.SockFilter
			err := binary.Read(f, binary.LittleEndian, &filter)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			filters = append(filters, filter)
		}
		f.Close()
		eval := func(call, architecture, family uint32) uint32 {
			var a uint32
			for pc := 0; pc < len(filters); pc++ {
				i := filters[pc]
				switch i.Code {
				case 0x20:
					switch i.K {
					case 0:
						a = call
					case 4:
						a = architecture
					case 16:
						a = family
					default:
						t.Fatal("offset")
					}
				case 0x15:
					if a == i.K {
						pc += int(i.Jt)
					} else {
						pc += int(i.Jf)
					}
				case 0x45:
					if a&i.K != 0 {
						pc += int(i.Jt)
					} else {
						pc += int(i.Jf)
					}
				case 0x06:
					return i.K
				default:
					t.Fatal("opcode")
				}
			}
			t.Fatal("no return")
			return 0
		}
		pair := uint32(53)
		if runtime.GOARCH == "arm64" {
			pair = 199
		}
		if eval(pair, arch, 1) != 0x50001 {
			t.Fatal("socketpair datagram bypass")
		}
		if eval(425, arch, 0) != 0x50001 {
			t.Fatal("io_uring socket bypass")
		}
		if eval(nr, arch, unix.AF_UNIX) != 0x50001 {
			t.Fatal("unix socket allowed")
		}
		want := uint32(0x7fff0000)
		if network == NetworkDeny {
			want = 0x50001
		}
		if eval(nr, arch, unix.AF_INET) != want {
			t.Fatal("inet socket wrong decision")
		}
		if eval(1, arch, 0) != 0x7fff0000 {
			t.Fatal("ordinary syscall blocked")
		}
		if eval(nr, 0, unix.AF_INET) != 0x80000000 {
			t.Fatal("wrong arch allowed")
		}
		if runtime.GOARCH == "amd64" && eval(nr|0x40000000, arch, unix.AF_INET) != 0x80000000 {
			t.Fatal("x32 bypass")
		}
	}
}
