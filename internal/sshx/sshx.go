// Package sshx is the only package allowed to construct SSH clients.
// Its single load-bearing idea: ssh.ClientConfig.Timeout covers
// TCP dial only — bounding the banner exchange, key exchange, and auth
// requires an explicit socket deadline. Plain ssh.Dial silently reintroduces
// the unbounded-hang failure mode runny exists to kill.
package sshx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// ErrAuthRejected marks a connection attempt that reached the server and
// completed the key exchange, but whose authentication the server refused.
// It distinguishes "the guest rejected our credential" from "ssh is not up
// yet" — the two look identical as raw handshake errors but mean opposite
// things to a caller mid-rotation. WaitFor deliberately keeps retrying on it:
// a guest reloading sshd can briefly reject the new credential before the
// flipped config takes effect.
var ErrAuthRejected = errors.New("ssh auth rejected")

// ErrSessionOpen marks a failure to open a session channel — the exec never
// started, so no command bytes reached the guest. It distinguishes "the
// transport is down" (the channel-open await timed out or the connection
// dropped) from "the command ran and failed". internal/guest translates only
// this into statemachine.ErrGuestUnreachable: a mid-job injection that fails
// at session-open provably never touched the guest, so the job is untouched
// and the slot is not redialed (decision 18). Everything past session-open
// stays ambiguous.
var ErrSessionOpen = errors.New("ssh session open failed")

// Config carries guest credentials and the per-attempt budget.
type Config struct {
	User     string
	Password string
	// Signer, when set, makes publickey the ONLY auth method attempted —
	// Password is ignored entirely, never a fallback. A silent fallback would
	// reintroduce the password-on-the-wire exposure while reporting the
	// hardened path succeeded; a guest that rejects the key must fail loudly
	// (ErrAuthRejected), not quietly downgrade.
	Signer ssh.Signer
	// HostKeys, when non-empty, pins the server: the handshake fails unless
	// the presented host key is a member of this set. The set must contain
	// EVERY key the server may present (capture all of /etc/ssh/*.pub, not
	// just one): which key the server offers is negotiated per connection, so
	// a partial set fails whenever negotiation lands outside it. Empty
	// preserves the no-verification default for ephemeral guests with fresh
	// keys every boot.
	HostKeys []ssh.PublicKey
	// Timeout bounds one connection attempt end-to-end: TCP connect, banner,
	// handshake, and auth together.
	Timeout time.Duration
}

// auth returns the exclusive auth method selection (see Config.Signer).
func (cfg Config) auth() []ssh.AuthMethod {
	if cfg.Signer != nil {
		return []ssh.AuthMethod{ssh.PublicKeys(cfg.Signer)}
	}
	return []ssh.AuthMethod{ssh.Password(cfg.Password)}
}

// hostKeyCallback returns the set-membership pin check, or the ephemeral
// no-verification default (see Config.HostKeys).
func (cfg Config) hostKeyCallback() ssh.HostKeyCallback {
	if len(cfg.HostKeys) == 0 {
		return ssh.InsecureIgnoreHostKey() // ephemeral guests, fresh keys every boot
	}
	pinned := make([][]byte, len(cfg.HostKeys))
	for i, k := range cfg.HostKeys {
		pinned[i] = k.Marshal()
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		got := key.Marshal()
		for _, want := range pinned {
			if bytes.Equal(got, want) {
				return nil
			}
		}
		return fmt.Errorf("host key for %s: presented %s key is not in the pinned set", hostname, key.Type())
	}
}

// Client wraps ssh.Client; obtain one only via Dial or WaitFor. The raw
// net.Conn is retained because the deadline recipe applies per operation,
// not just at dial time: channel opens and exec requests await server
// replies, and a guest that wedges with the TCP connection still open
// blocks them forever unless the socket itself bounds the wait.
type Client struct {
	c       *ssh.Client
	conn    net.Conn
	timeout time.Duration
}

// Dial performs one bounded connection attempt: TCP connect under a dial
// timeout, then an explicit socket deadline over banner + key exchange + auth.
func Dial(ctx bounded.Context, addr string, cfg Config) (*Client, error) {
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
		Auth:            cfg.auth(),
		HostKeyCallback: cfg.hostKeyCallback(),
		Timeout:         cfg.Timeout,
	})
	if err != nil {
		_ = conn.Close()
		// x/crypto/ssh exposes client auth failure only as error text. The
		// full-prefix match (not Contains) is deliberate: a disconnect error
		// embeds server-controlled text ("ssh: disconnect, reason N: <msg>"),
		// which a hostile guest could lace with the auth string — but never
		// at position zero. Upstream's own tests assert this exact prefix,
		// and ours run against every dependency bump, so a rewording fails
		// loudly instead of silently degrading rejection into absence.
		if strings.HasPrefix(err.Error(), "ssh: handshake failed: ssh: unable to authenticate") {
			return nil, fmt.Errorf("ssh handshake %s: %w: %w", addr, ErrAuthRejected, err)
		}
		return nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	// Clear the deadline for session use; per-operation bounds come from ctx.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = sc.Close()
		return nil, fmt.Errorf("ssh clear deadline: %w", err)
	}
	return &Client{c: ssh.NewClient(sc, chans, reqs), conn: conn, timeout: cfg.Timeout}, nil
}

// WaitFor retries Dial every interval until success or ctx expiry. The
// AWAIT_SSH state is this function plus a state deadline. Auth rejection
// (ErrAuthRejected) retries like any other failure — sshd may still be
// flipping its config when the first post-rotation attempt lands — and the
// expiry error carries the last attempt's text, so a deadline spent being
// rejected reads as rejection, not absence.
func WaitFor(ctx bounded.Context, addr string, cfg Config, interval time.Duration) (*Client, error) {
	return waitFor(ctx, addr, interval, func() (*Client, error) { return Dial(ctx, addr, cfg) })
}

// waitFor is WaitFor's retry loop with the per-attempt dial pulled out as a
// parameter — Dial in production, a fake in tests — so the "which error
// survives to the expiry message" bookkeeping is unit-testable without a
// real network attempt in the timing-critical path. A real attempt's own
// socket deadline stays covered by TestDial* against a real server; this
// only exercises the loop.
func waitFor(ctx bounded.Context, addr string, interval time.Duration, dial func() (*Client, error)) (*Client, error) {
	var lastErr error
	for {
		c, err := dial()
		if err == nil {
			return c, nil
		}
		// An attempt aborted by ctx expiry mid-dial reports only the abort
		// ("i/o timeout", "operation was canceled") — keep the last attempt
		// that failed on its own merits instead; that is the one that says
		// WHY ssh never came up (rejection vs refusal vs silence).
		if ctx.Err() == nil || lastErr == nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			// lastErr is deliberately %v, not %w: a per-attempt TCP timeout
			// satisfies errors.Is(err, context.DeadlineExceeded), and letting
			// it ride the chain would make the FSM record an operator-canceled
			// wait as a deadline expiry. The text alone carries the diagnosis.
			return nil, fmt.Errorf("waiting for ssh on %s: %w (last attempt: %v)", addr, ctx.Err(), lastErr)
		case <-time.After(interval):
		}
	}
}

// Close tears down the connection. The polite SSH disconnect is a write — a
// wedged transport blocks it forever, and Close runs on the teardown path
// that must not wedge — so the socket is cut first; the client then cleans
// up quickly against the dead transport.
func (c *Client) Close() error {
	err := c.conn.Close()
	_ = c.c.Close()
	return err
}

// newSession opens a session channel under a socket deadline: channel open
// awaits a server reply, the same blind spot as the banner exchange.
// A wedged guest fails here within timeout instead of hanging
// the caller forever.
func (c *Client) newSession() (*ssh.Session, error) {
	_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	return c.c.NewSession()
}

// maxOutput caps the bytes Output buffers from one command. Output reads the
// full combined stdout+stderr, bounded by the per-call deadline and the
// teardown socket-cut — but not by bytes: a guest controlling how many
// _diag/*.log files exist (PullDiag) could force a large transient allocation
// on every failure teardown. Sized well above any healthy Output — PullDiag,
// the largest caller, tails 32 KB from a handful of files (a few hundred KB).
const maxOutput = 4 << 20

// capBuf collects up to max bytes of combined output and silently discards the
// rest. It always reports a full write so the session's stdout/stderr io.Copy
// is never short-write errored — the goal is to bound the buffer, not to
// backpressure the guest. The mutex guards concurrent stdout+stderr writes
// (the session pumps them from separate goroutines), exactly as x/crypto/ssh's
// own CombinedOutput buffer does.
type capBuf struct {
	mu  sync.Mutex
	b   bytes.Buffer
	max int
}

func (c *capBuf) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// Output runs cmd and captures combined stdout+stderr, bounded by ctx and
// capped at maxOutput bytes. Used for short provisioning steps and post-mortem
// pulls.
func (c *Client) Output(ctx bounded.Context, cmd string) ([]byte, int, error) {
	sess, err := c.newSession()
	if err != nil {
		// Session-open failed: the exec never started, so no bytes reached the
		// guest. Mark it ErrSessionOpen so a caller mid-job can tell "provably
		// never sent" from "ran and failed" (decision 18).
		return nil, -1, fmt.Errorf("ssh session: %w: %w", ErrSessionOpen, err)
	}

	type result struct {
		out  []byte
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		buf := capBuf{max: maxOutput}
		sess.Stdout, sess.Stderr = &buf, &buf
		err := sess.Run(cmd)
		_ = sess.Close()
		done <- result{buf.b.Bytes(), exitCode(err), err}
	}()
	select {
	case <-ctx.Done():
		// Session close is a write; on the wedged transport that got us
		// here it can block forever — detach it. The worker unblocks when
		// the close lands or when Client.Close cuts the socket.
		go func() { _ = sess.Close() }()
		// Never echo cmd: it may carry a secret (the JIT registration blob
		// rides inside the provision script). The caller knows which step ran.
		return nil, -1, fmt.Errorf("ssh command timed out: %w", ctx.Err())
	case r := <-done:
		if r.err != nil && r.code < 0 {
			return r.out, r.code, fmt.Errorf("ssh command failed: %w", r.err)
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

	sess    *ssh.Session
	wait    chan error
	done    chan struct{}
	endOnce sync.Once
}

// end is the proc's single exit path, idempotent: it releases every proc
// goroutine (readers blocked on a full Lines channel included) and closes
// the session without ever blocking on the wire — channel-close is a write,
// and Kill runs on the teardown path that must not wedge. A close stuck on
// a dead transport parks in its goroutine until Client.Close cuts the
// socket, which teardown always does right after Kill.
func (p *Proc) end() {
	p.endOnce.Do(func() {
		close(p.done)
		go func() { _ = p.sess.Close() }()
	})
}

// Start launches cmd and streams its output. Cancel ctx (or call Kill) to
// end the session; every goroutine started here exits by teardown, not by
// daemon shutdown — one leaked watcher per cycle was an unbounded leak in a
// daemon that cycles every few minutes for weeks.
//
// stdin, when non-nil, is fed to the remote command's standard input. It is the
// channel for input that must stay OUT of cmd — notably a secret like the JIT
// config: x/crypto folds cmd into its exec error on a server-side reject, and
// that error reaches cycle.json and the gRPC surface, so a secret in cmd would
// leak there. The invariant: secrets travel over stdin, never the command.
//
// Start deliberately takes a plain context (not bounded.Context): the ctx is
// the proc's LIFETIME — run.sh must outlive the caller's state deadline —
// not an operation bound. Establishment is bounded internally by the socket
// deadline below.
func (c *Client) Start(ctx context.Context, cmd string, stdin io.Reader) (*Proc, error) {
	// One deadline bracket covers the whole establishment — channel open,
	// pipes, exec request, and the error-path closes, which are writes a
	// wedged transport would otherwise block forever. Cleared on return;
	// Start returns within microseconds of the exec request landing.
	_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	sess, err := c.c.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh session: %w", err)
	}
	if stdin != nil {
		sess.Stdin = stdin
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
		// Never echo cmd: it may carry a secret (the JIT registration blob
		// rides inside the provision script). The caller knows which step ran.
		return nil, fmt.Errorf("ssh start failed: %w", err)
	}

	lines := make(chan string, 64)
	p := &Proc{Lines: lines, sess: sess, wait: make(chan error, 1), done: make(chan struct{})}

	readDone := make(chan struct{}, 2)
	for _, r := range []io.Reader{stdout, stderr} {
		go func() {
			readLines(r, lines, p.done)
			readDone <- struct{}{}
		}()
	}

	go func() {
		err := sess.Wait()
		<-readDone
		<-readDone
		close(lines)
		p.wait <- err
		p.end() // natural exit: release the ctx watcher below
	}()
	go func() {
		select {
		case <-ctx.Done():
			p.end()
		case <-p.done:
		}
	}()

	return p, nil
}

// maxLine caps one delivered output line. Longer lines are truncated, not
// fatal: bufio.Scanner's ErrTooLong silently stopped the reader, so a single
// oversized line (a runner dumping a blob) made the FSM miss every later
// marker — the JOB state then burned its whole budget on a finished job.
const maxLine = 1 << 20

// readLines streams r into lines until EOF/error, truncating oversized lines
// and bailing out when done closes (nobody drains Lines after the FSM sees
// its marker; without the escape a chatty runner wedged the readers on a
// full channel forever).
func readLines(r io.Reader, lines chan<- string, done <-chan struct{}) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		var buf []byte
		truncated := false
		for {
			chunk, isPrefix, err := br.ReadLine()
			if err != nil {
				return // EOF or transport error; a trailing partial line is teardown noise
			}
			if room := maxLine - len(buf); room > 0 {
				take := min(len(chunk), room)
				buf = append(buf, chunk[:take]...)
				truncated = truncated || take < len(chunk)
			} else {
				truncated = true
			}
			if !isPrefix {
				break
			}
		}
		text := string(buf)
		if truncated {
			text += " …[truncated]"
		}
		select {
		case lines <- text:
		case <-done:
			return
		}
	}
}

// Wait blocks until the command exits and returns its exit code. A negative
// code means the session died without one (connection loss, kill). Call it
// only after Lines closes (the FSM's pattern): before that, a wedged
// transport can hold the session's exit indefinitely, until Client.Close
// cuts the socket.
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

// Kill tears the session down without blocking (see end).
func (p *Proc) Kill() { p.end() }

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
