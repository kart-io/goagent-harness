package sshsandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// OpenSSH 的 MaxSessions 默认是 10，留出余量给并发的连接检查。
	maxSessions       = 8
	keepaliveInterval = 15 * time.Second
	keepaliveTimeout  = 10 * time.Second
)

// errDial 表示没有连上远端，这时命令一定没有发出去。
var errDial = errors.New("ssh dial failed")

type client struct {
	cfg   Config
	slots chan struct{}

	mu     sync.Mutex
	conn   *ssh.Client
	closed bool
}

func newClient(cfg Config) *client {
	return &client{cfg: cfg, slots: make(chan struct{}, maxSessions)}
}

func (c *client) connection(ctx context.Context) (*ssh.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("ssh client is closed")
	}
	if c.conn != nil {
		return c.conn, nil
	}
	if c.cfg.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.ConnectTimeout)
		defer cancel()
	}
	var dialer net.Dialer
	raw, err := dialer.DialContext(ctx, "tcp", c.cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errDial, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	sshConn, channels, requests, err := ssh.NewClientConn(raw, c.cfg.Addr, &ssh.ClientConfig{User: c.cfg.User, Auth: c.cfg.Auth, HostKeyCallback: c.cfg.HostKeyCallback})
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("%w: %v", errDial, err)
	}
	_ = raw.SetDeadline(time.Time{})
	conn := ssh.NewClient(sshConn, channels, requests)
	c.conn = conn
	go c.keepalive(conn)
	return conn, nil
}

// keepalive 发现静默断开的连接。否则没有超时的命令会一直等下去。
func (c *client) keepalive(conn *ssh.Client) {
	done := make(chan struct{})
	go func() {
		_ = conn.Wait()
		c.forget(conn)
		close(done)
	}()
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
		answered := make(chan error, 1)
		go func() {
			_, _, err := conn.SendRequest("keepalive@openssh.com", true, nil)
			answered <- err
		}()
		select {
		case <-done:
			return
		case err := <-answered:
			if err == nil {
				continue
			}
		case <-time.After(keepaliveTimeout):
		}
		_ = conn.Close()
	}
}

func (c *client) forget(conn *ssh.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == conn {
		c.conn = nil
	}
}

func (c *client) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.conn == nil {
		return nil
	}
	conn := c.conn
	c.conn = nil
	return conn.Close()
}

// session 开启一个会话。这一步还没有发出命令，连接已失效时可以换新连接重试。
func (c *client) session(ctx context.Context) (*ssh.Session, func(), error) {
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	release := func() { <-c.slots }
	var last error
	for range 2 {
		conn, err := c.connection(ctx)
		if err != nil {
			release()
			return nil, nil, err
		}
		type opened struct {
			s   *ssh.Session
			err error
		}
		result := make(chan opened, 1)
		go func() {
			s, err := conn.NewSession()
			result <- opened{s, err}
		}()
		select {
		case r := <-result:
			if r.err == nil {
				return r.s, release, nil
			}
			last = r.err
			// 只有连接失效才换连接。会话被拒绝时连接还在用，不能关。
			var rejected *ssh.OpenChannelError
			if errors.As(r.err, &rejected) {
				release()
				return nil, nil, r.err
			}
			_ = conn.Close()
			c.forget(conn)
		case <-ctx.Done():
			release()
			return nil, nil, ctx.Err()
		}
	}
	release()
	return nil, nil, last
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.b.Bytes())
}

// run 运行一条命令行。keepStdin 让远端的 stdin 一直打开到会话结束，远端脚本用它感知宿主取消或断线。
func (c *client) run(ctx context.Context, cmdline string, stdin []byte, keepStdin bool) (stdout, stderr []byte, code int, err error) {
	s, release, err := c.session(ctx)
	if err != nil {
		return nil, nil, -1, err
	}
	defer release()
	defer func() { _ = s.Close() }()
	var out, errOut lockedBuffer
	s.Stdout, s.Stderr = &out, &errOut
	if keepStdin {
		pipe, err := s.StdinPipe()
		if err != nil {
			return nil, nil, -1, err
		}
		defer func() { _ = pipe.Close() }()
	} else {
		s.Stdin = bytes.NewReader(stdin)
	}
	finished := make(chan error, 1)
	go func() { finished <- s.Run(cmdline) }()
	select {
	case <-ctx.Done():
		return out.bytes(), errOut.bytes(), -1, ctx.Err()
	case err = <-finished:
	}
	if err == nil {
		return out.bytes(), errOut.bytes(), 0, nil
	}
	var exited *ssh.ExitError
	if errors.As(err, &exited) {
		return out.bytes(), errOut.bytes(), exited.ExitStatus(), nil
	}
	return out.bytes(), errOut.bytes(), -1, err
}
