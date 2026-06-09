// Package sshx is the only package allowed to construct SSH clients
// (ADR-0002). Its single load-bearing idea: ssh.ClientConfig.Timeout covers
// TCP dial only — bounding the banner exchange, key exchange, and auth
// requires an explicit socket deadline. Plain ssh.Dial silently reintroduces
// the unbounded-hang failure mode runny exists to kill.
package sshx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// Config carries guest credentials and the per-attempt budget.
type Config struct {
	User     string
	Password string
	// Timeout bounds one connection attempt end-to-end: TCP connect, banner,
	// handshake, and auth together.
	Timeout time.Duration
}

// Client wraps ssh.Client; obtain one only via Dial or WaitFor.
type Client struct {
	c *ssh.Client
}

// Dial performs one bounded connection attempt (the ADR-0002 recipe).
func Dial(ctx context.Context, addr string, cfg Config) (*Client, error) {
	d := net.Dialer{Timeout: cfg.Timeout}
	conn, err := d.DialContext(ctx, "tcp", addr) // bounds TCP connect
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	// Bounds banner + key exchange + auth. Without this, a server that
	// accepts TCP but never sends its banner hangs the client forever.
	if err := conn.SetDeadline(time.Now().Add(cfg.Timeout)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh set deadline: %w", err)
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.Password(cfg.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // ephemeral guests, fresh keys every boot
		Timeout:         cfg.Timeout,
	})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	// Clear the deadline for session use; per-operation bounds come from ctx.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = sc.Close()
		return nil, fmt.Errorf("ssh clear deadline: %w", err)
	}
	return &Client{c: ssh.NewClient(sc, chans, reqs)}, nil
}

// WaitFor retries Dial every interval until success or ctx expiry. The
// AWAIT_SSH state is this function plus a state deadline.
func WaitFor(ctx context.Context, addr string, cfg Config, interval time.Duration) (*Client, error) {
	var lastErr error
	for {
		c, err := Dial(ctx, addr, cfg)
		if err == nil {
			return c, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for ssh on %s: %w (last attempt: %v)", addr, ctx.Err(), lastErr)
		case <-time.After(interval):
		}
	}
}

func (c *Client) Close() error { return c.c.Close() }

// Output runs cmd and captures combined stdout+stderr, bounded by ctx. Used
// for short provisioning steps and post-mortem pulls.
func (c *Client) Output(ctx context.Context, cmd string) ([]byte, int, error) {
	sess, err := c.c.NewSession()
	if err != nil {
		return nil, -1, fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	type result struct {
		out  []byte
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		out, err := sess.CombinedOutput(cmd)
		done <- result{out, exitCode(err), err}
	}()
	select {
	case <-ctx.Done():
		_ = sess.Close() // unblocks CombinedOutput
		return nil, -1, fmt.Errorf("ssh command %q: %w", cmd, ctx.Err())
	case r := <-done:
		if r.err != nil && r.code < 0 {
			return r.out, r.code, fmt.Errorf("ssh command %q: %w", cmd, r.err)
		}
		return r.out, r.code, nil
	}
}

// Proc is a long-running remote command (the runner's run.sh) with its output
// streamed line-by-line — StdoutPipe + reader goroutine, so output arrives
// incrementally and the 64KB pipe-buffer deadlock class cannot occur.
type Proc struct {
	// Lines receives each output line (stdout and stderr interleaved). Closed
	// when the command exits or the connection drops.
	Lines <-chan string

	sess *ssh.Session
	wait chan error
}

// Start launches cmd and streams its output. Cancel ctx to kill the session.
func (c *Client) Start(ctx context.Context, cmd string) (*Proc, error) {
	sess, err := c.c.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh session: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("ssh stdout pipe: %w", err)
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("ssh stderr pipe: %w", err)
	}
	if err := sess.Start(cmd); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("ssh start %q: %w", cmd, err)
	}

	lines := make(chan string, 64)
	readDone := make(chan struct{}, 2)
	for _, r := range []interface{ Read([]byte) (int, error) }{stdout, stderr} {
		go func() {
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				select {
				case lines <- sc.Text():
				case <-ctx.Done():
					readDone <- struct{}{}
					return
				}
			}
			readDone <- struct{}{}
		}()
	}

	wait := make(chan error, 1)
	go func() {
		err := sess.Wait()
		<-readDone
		<-readDone
		close(lines)
		wait <- err
	}()
	go func() {
		<-ctx.Done()
		_ = sess.Close() // unblocks Wait and the readers
	}()

	return &Proc{Lines: lines, sess: sess, wait: wait}, nil
}

// Wait blocks until the command exits and returns its exit code. A negative
// code means the session died without one (connection loss, kill).
func (p *Proc) Wait() (int, error) {
	err := <-p.wait
	if err == nil {
		return 0, nil
	}
	if code := exitCode(err); code >= 0 {
		return code, nil
	}
	return -1, fmt.Errorf("remote command did not exit cleanly: %w", err)
}

// Kill tears the session down.
func (p *Proc) Kill() { _ = p.sess.Close() }

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return ee.ExitStatus()
	}
	return -1
}
