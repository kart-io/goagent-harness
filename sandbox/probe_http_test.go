package sandbox

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Sending a response and closing before the request arrives makes curl fail
// with EPIPE on Linux, falsely reporting that a working backend is unavailable.
func TestProbeHTTPWaitsForRequest(t *testing.T) {
	for _, network := range []string{"tcp4", "unix"} {
		t.Run(network, func(t *testing.T) {
			address := "127.0.0.1:0"
			if network == "unix" {
				dir, err := os.MkdirTemp("/tmp", "harness-http-test-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
				address = filepath.Join(dir, "s.sock")
			}
			listener, err := net.Listen(network, address)
			if err != nil {
				t.Fatal(err)
			}
			stop := startProbeHTTP(listener)
			defer stop()
			conn, err := net.Dial(network, listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
			var b [1]byte
			if _, err := conn.Read(b[:]); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("response arrived before request: %q %v", b, err)
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatal(response.Status)
			}
		})
	}
}
