package sandbox

import (
	"net"
	"net/http"
	"time"
)

func startProbeHTTP(listener net.Listener) func() {
	server := &http.Server{
		ReadHeaderTimeout: time.Second,
		WriteTimeout:      time.Second,
		IdleTimeout:       time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() { _ = server.Serve(listener) }()
	return func() {
		_ = server.Close()
		_ = listener.Close()
	}
}
