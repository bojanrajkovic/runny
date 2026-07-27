package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// testServer is a minimal in-process SSH server: password auth, exec with
// canned behaviors. It exists so the deadline recipe is tested against real
// SSH wire behavior, not mocks.
func testServer(t *testing.T) string {
	t.Helper()
	addr, _ := serveSSH(t, &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if meta.User() == "admin" && string(pass) == "admin" {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	})
	return addr
}

// newTestSigner mints an in-memory ed25519 signer (both server host keys and
// client credentials in these tests).
func newTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// serveSSH runs an in-process SSH server with the given auth config and the
// canned exec behaviors, returning its address and host public key.
func serveSSH(t *testing.T, conf *ssh.ServerConfig) (string, ssh.PublicKey) {
	t.Helper()
	hostKey := newTestSigner(t)
	conf.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(conn, conf)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for newCh := range chans {
					if newCh.ChannelType() != "session" {
						_ = newCh.Reject(ssh.UnknownChannelType, "")
						continue
					}
					ch, chReqs, err := newCh.Accept()
					if err != nil {
						continue
					}
					go handleSession(ch, chReqs)
				}
			}()
		}
	}()
	return ln.Addr().String(), hostKey.PublicKey()
}

func handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Cmd string }
		_ = ssh.Unmarshal(req.Payload, &payload)
		_ = req.Reply(true, nil)

		exit := func(code uint32) {
			b := ssh.Marshal(struct{ C uint32 }{code})
			_, _ = ch.SendRequest("exit-status", false, b)
		}
		switch {
		case payload.Cmd == "hello":
			fmt.Fprintln(ch, "hello world")
			exit(0)
		case payload.Cmd == "fail":
			fmt.Fprintln(ch.Stderr(), "boom")
			exit(3)
		case payload.Cmd == "stream":
			for i := range 3 {
				fmt.Fprintf(ch, "line %d\n", i)
				time.Sleep(20 * time.Millisecond)
			}
			exit(0)
		case payload.Cmd == "longline":
			// A single oversized line, then a marker: the reader must
			// truncate and keep going, not die silently.
			fmt.Fprintf(ch, "%s\n", strings.Repeat("x", 3<<20))
			fmt.Fprintln(ch, "marker after long line")
			exit(0)
		case payload.Cmd == "flood":
			// Far more than the client's Output byte cap, then exit cleanly: the
			// cap must bound the buffer even when the command completes normally.
			chunk := strings.Repeat("x", 1<<20)
			for range 8 { // 8 MiB total
				fmt.Fprint(ch, chunk)
			}
			exit(0)
		case payload.Cmd == "spew":
			// Far more output than the client's Lines buffer, then hang:
			// a chatty runner whose consumer has stopped draining.
			for i := range 500 {
				fmt.Fprintf(ch, "spew %d\n", i)
			}
			select {} // never exits; the client must Kill it
		case payload.Cmd == "catstdin":
			// Echo the channel's stdin (what the client sent via Session.Stdin)
			// back as stdout, until the client closes its write side (EOF).
			_, _ = io.Copy(ch, ch)
			exit(0)
		case strings.HasPrefix(payload.Cmd, "hang"):
			select {} // never exits; the client must bound it
		default:
			exit(127)
		}
		return
	}
}

// testCtx satisfies the bounded.Context the client API demands.
func testCtx(t *testing.T) bounded.Context {
	t.Helper()
	ctx, cancel := bounded.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

var testCfg = Config{User: "admin", Password: "admin", Timeout: 2 * time.Second}

func TestDialAndOutput(t *testing.T) {
	addr := testServer(t)
	c, err := Dial(testCtx(t), addr, testCfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	out, code, err := c.Output(testCtx(t), "hello")
	if err != nil || code != 0 {
		t.Fatalf("Output: %q, %d, %v", out, code, err)
	}
	if !strings.Contains(string(out), "hello world") {
		t.Errorf("out = %q", out)
	}
}

func TestOutputExitCode(t *testing.T) {
	c, err := Dial(testCtx(t), testServer(t), testCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, code, err := c.Output(testCtx(t), "fail")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if code != 3 || !strings.Contains(string(out), "boom") {
		t.Errorf("code=%d out=%q, want 3/boom", code, out)
	}
}

func TestDialWrongPassword(t *testing.T) {
	_, err := Dial(testCtx(t), testServer(t), Config{User: "admin", Password: "nope", Timeout: 2 * time.Second})
	if err == nil {
		t.Fatal("want auth failure")
	}
	// Rejection must be distinguishable from "ssh not up" (the rotation
	// redial's signal).
	if !errors.Is(err, ErrAuthRejected) {
		t.Errorf("err = %v, want ErrAuthRejected in the chain", err)
	}
}

// TestDialBannerHang is THE test: a server that accepts TCP but never sends
// its SSH banner. ssh(1) has no option that bounds this; the recipe must.
func TestDialBannerHang(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn // hold open, never write
		}
	}()

	start := time.Now()
	_, err = Dial(testCtx(t), ln.Addr().String(), Config{User: "admin", Password: "admin", Timeout: 500 * time.Millisecond})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want banner-hang failure")
	}
	if elapsed > 2*time.Second {
		t.Errorf("dial took %v; the deadline did not bound the banner exchange", elapsed)
	}
	// A server that never spoke did not reject anything.
	if errors.Is(err, ErrAuthRejected) {
		t.Errorf("banner hang classified as auth rejection: %v", err)
	}
}

func TestOutputBoundedByContext(t *testing.T) {
	c, err := Dial(testCtx(t), testServer(t), testCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := bounded.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = c.Output(ctx, "hang")
	if err == nil {
		t.Fatal("want context expiry")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("Output not bounded by ctx: took %v", time.Since(start))
	}
}

func TestStartStreams(t *testing.T) {
	c, err := Dial(testCtx(t), testServer(t), testCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	p, err := c.Start(t.Context(), "stream", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var got []string
	for line := range p.Lines {
		got = append(got, line)
	}
	code, err := p.Wait()
	if err != nil || code != 0 {
		t.Fatalf("Wait: %d, %v", code, err)
	}
	if len(got) != 3 || got[0] != "line 0" {
		t.Errorf("lines = %v", got)
	}
}

func TestStartKilledByContext(t *testing.T) {
	c, err := Dial(testCtx(t), testServer(t), testCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	p, err := c.Start(ctx, "hang", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = p.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after ctx cancellation — the hang class is back")
	}
}

// wedgedServer completes the SSH handshake but never answers channel opens —
// the post-handshake analogue of the banner hang: a guest that wedged with
// its TCP connection still alive.
func wedgedServer(t *testing.T) string {
	t.Helper()
	conf := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil },
	}
	conf.AddHostKey(newTestSigner(t))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(conn, conf)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for range chans {
					// Take the open into limbo: never Accept, never Reject.
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// Channel open is a request the server must answer; the socket deadline must
// bound it the same way it bounds the banner (TestDialBannerHang's sibling).
func TestOutputBoundedOnWedgedChannelOpen(t *testing.T) {
	cfg := Config{User: "admin", Password: "admin", Timeout: 500 * time.Millisecond}
	c, err := Dial(testCtx(t), wedgedServer(t), cfg)
	if err != nil {
		t.Fatalf("Dial (handshake should succeed): %v", err)
	}
	defer c.Close()
	start := time.Now()
	_, _, err = c.Output(testCtx(t), "hello")
	if err == nil {
		t.Fatal("want channel-open failure against a wedged guest")
	}
	// A session-open failure is exactly the "provably never sent" case that
	// internal/guest maps to ErrGuestUnreachable (issue #39, decision 18).
	if !errors.Is(err, ErrSessionOpen) {
		t.Errorf("Output channel-open failure not marked ErrSessionOpen: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Output took %v; the deadline did not bound the channel open", elapsed)
	}
}

func TestStartBoundedOnWedgedChannelOpen(t *testing.T) {
	cfg := Config{User: "admin", Password: "admin", Timeout: 500 * time.Millisecond}
	c, err := Dial(testCtx(t), wedgedServer(t), cfg)
	if err != nil {
		t.Fatalf("Dial (handshake should succeed): %v", err)
	}
	defer c.Close()
	start := time.Now()
	_, err = c.Start(t.Context(), "hello", nil)
	if err == nil {
		t.Fatal("want session failure against a wedged guest")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Start took %v; the deadline did not bound establishment", elapsed)
	}
}

// Start feeds its stdin arg to the remote command — the channel for input that
// must stay OUT of the command string (the JIT config). Round-trip a payload
// through a guest that echoes its stdin to stdout.
func TestStartDeliversStdin(t *testing.T) {
	c, err := Dial(testCtx(t), testServer(t), testCfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	const payload = "JITSECRET-deadbeef"
	p, err := c.Start(t.Context(), "catstdin", strings.NewReader(payload+"\n"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var got strings.Builder
	for line := range p.Lines {
		got.WriteString(line)
	}
	if code, err := p.Wait(); err != nil || code != 0 {
		t.Fatalf("Wait: %d, %v", code, err)
	}
	if got.String() != payload {
		t.Errorf("stdin not delivered to the command: got %q, want %q", got.String(), payload)
	}
}

// When Output overflows its cap, the END of the output is what survives.
// PullDiag streams whole runner logs, and a log's ending is the failure that
// teardown pulled it for — keeping the first N bytes would discard exactly
// the part worth having and leave the runner's startup chatter behind.
func TestCapBufKeepsTheTailNotTheHead(t *testing.T) {
	b := capBuf{max: 10}
	if _, err := b.Write([]byte("0123456789")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := b.Write([]byte("ABCDE")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := string(b.bytes())
	if !strings.HasSuffix(got, "56789ABCDE") {
		t.Errorf("cap kept the wrong end: %q, want it to end in %q", got, "56789ABCDE")
	}
	if strings.Contains(got, "01234") {
		t.Errorf("the oldest bytes must be the ones dropped: %q", got)
	}
}

// A truncated capture must say so. capBuf reports a full write regardless, so
// without a marker in the bytes themselves a clipped post-mortem is
// indistinguishable from a log that simply ended there.
func TestCapBufMarksTruncation(t *testing.T) {
	b := capBuf{max: 4}
	if _, err := b.Write([]byte("aaaaBBBB")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := string(b.bytes())
	if !strings.Contains(got, "runny:") {
		t.Errorf("truncated output carries no marker: %q", got)
	}
	if !strings.Contains(got, "4") {
		t.Errorf("marker should report how many bytes were dropped: %q", got)
	}
	if !strings.HasSuffix(got, "BBBB") {
		t.Errorf("marker must precede the kept tail, not replace it: %q", got)
	}
}

// The common case must stay byte-identical: no marker, no reordering, nothing
// added for a capture that fit.
func TestCapBufUnderCapIsUntouched(t *testing.T) {
	b := capBuf{max: 1024}
	if _, err := b.Write([]byte("hello world")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := string(b.bytes()); got != "hello world" {
		t.Errorf("under-cap output altered: %q", got)
	}
}

// A multi-megabyte capture must arrive whole. PullDiag streams the runner's
// _diag logs unabridged, so anything under the cap has to survive the pipe
// intact — not merely stay under it. The cap's own overflow behaviour is
// pinned directly on capBuf above; driving 64 MiB through the test server to
// re-prove it here would buy nothing for the runtime.
func TestOutputDeliversLargeCaptureIntact(t *testing.T) {
	c, err := Dial(testCtx(t), testServer(t), testCfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	out, code, err := c.Output(testCtx(t), "flood")
	if err != nil || code != 0 {
		t.Fatalf("Output: code %d, err %v", code, err)
	}
	if want := 8 << 20; len(out) != want {
		t.Errorf("Output delivered %d bytes, want the full %d", len(out), want)
	}
	if len(out) > maxOutput {
		t.Errorf("Output exceeded its cap: %d bytes (want <= %d)", len(out), maxOutput)
	}
	if strings.Contains(string(out), "runny:") {
		t.Errorf("an under-cap capture must carry no truncation marker")
	}
}

// A single oversized output line must not kill the readers: bufio.Scanner's
// ErrTooLong once silently ended scanning, so every later line — including
// the completion markers the FSM watches for — was lost.
func TestStartSurvivesOversizedLine(t *testing.T) {
	c, err := Dial(testCtx(t), testServer(t), testCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p, err := c.Start(t.Context(), "longline", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var got []string
	for line := range p.Lines {
		got = append(got, line)
	}
	if code, err := p.Wait(); err != nil || code != 0 {
		t.Fatalf("Wait: %d, %v", code, err)
	}
	if len(got) != 2 {
		t.Fatalf("lines = %d, want 2 (truncated + marker)", len(got))
	}
	if len(got[0]) > maxLine+32 || !strings.HasSuffix(got[0], "…[truncated]") {
		t.Errorf("long line not truncated: len=%d suffix=%q", len(got[0]), got[0][max(0, len(got[0])-20):])
	}
	if got[1] != "marker after long line" {
		t.Errorf("marker lost after oversized line: %q", got[1])
	}
}

// Kill must release readers blocked on a full Lines channel — without the
// proc's own done signal they parked forever once the FSM stopped draining
// (it stops at the completion marker), wedging Wait behind them.
func TestKillUnblocksWedgedReaders(t *testing.T) {
	c, err := Dial(testCtx(t), testServer(t), testCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p, err := c.Start(t.Context(), "spew", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Let the readers fill the buffered channel and block; drain nothing.
	time.Sleep(300 * time.Millisecond)
	p.Kill()
	done := make(chan struct{})
	go func() {
		_, _ = p.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after Kill with un-drained output")
	}
}

// Proc goroutines must end at teardown, not daemon shutdown — one leaked
// ctx-watcher per cycle is unbounded growth in a daemon that cycles every
// few minutes for weeks.
func TestStartGoroutinesEndAtTeardown(t *testing.T) {
	c, err := Dial(testCtx(t), testServer(t), testCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	base := runtime.NumGoroutine()
	for range 10 {
		p, err := c.Start(t.Context(), "stream", nil)
		if err != nil {
			t.Fatal(err)
		}
		for range p.Lines {
		}
		if code, err := p.Wait(); err != nil || code != 0 {
			t.Fatalf("Wait: %d, %v", code, err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("proc goroutines leaked across cycles: %d before, %d after", base, runtime.NumGoroutine())
}

func TestWaitFor(t *testing.T) {
	addr := testServer(t)
	ctx, cancel := bounded.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	c, err := WaitFor(ctx, addr, testCfg, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	_ = c.Close()

	// And expiry against a dead address.
	ctx2, cancel2 := bounded.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel2()
	_, err = WaitFor(ctx2, "127.0.0.1:1", Config{User: "a", Password: "b", Timeout: 100 * time.Millisecond}, 50*time.Millisecond)
	if err == nil {
		t.Fatal("want WaitFor expiry on dead address")
	}
}

// acceptKey is a PublicKeyCallback accepting exactly one public key.
func acceptKey(want ssh.PublicKey) func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
	return func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(key.Marshal(), want.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("denied")
	}
}

// With a Signer set, password auth must never be attempted — a fallback would
// reintroduce the password on the wire while reporting the hardened path
// succeeded.
func TestDialSignerNeverAttemptsPassword(t *testing.T) {
	signer := newTestSigner(t)
	addr, _ := serveSSH(t, &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			t.Error("password auth attempted with Signer set")
			return nil, errors.New("denied")
		},
		PublicKeyCallback: acceptKey(signer.PublicKey()),
	})
	c, err := Dial(testCtx(t), addr, Config{User: "admin", Password: "admin", Signer: signer, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Dial with signer: %v", err)
	}
	_ = c.Close()
}

// The inverse: a guest that rejects the key must fail loudly even though the
// configured password would have worked. Auth selection is exclusive.
func TestDialNoPasswordFallback(t *testing.T) {
	addr, _ := serveSSH(t, &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if meta.User() == "admin" && string(pass) == "admin" {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, errors.New("denied")
		},
	})
	_, err := Dial(testCtx(t), addr, Config{User: "admin", Password: "admin", Signer: newTestSigner(t), Timeout: 2 * time.Second})
	if err == nil {
		t.Fatal("Dial succeeded — silent password fallback is back")
	}
	if !errors.Is(err, ErrAuthRejected) {
		t.Errorf("err = %v, want ErrAuthRejected", err)
	}
}

// Pinning is set-membership: any captured host key passes (the host-key
// algorithm is negotiated, so which key the server presents is its choice),
// and a server outside the set fails before auth.
func TestDialHostKeyPinning(t *testing.T) {
	passwordConf := func() *ssh.ServerConfig {
		return &ssh.ServerConfig{
			PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil },
		}
	}
	addr, hostKey := serveSSH(t, passwordConf())
	stranger := newTestSigner(t).PublicKey()

	// Member of the pin set (not the first entry — membership, not equality).
	cfg := Config{User: "admin", Password: "admin", Timeout: 2 * time.Second}
	cfg.HostKeys = []ssh.PublicKey{stranger, hostKey}
	c, err := Dial(testCtx(t), addr, cfg)
	if err != nil {
		t.Fatalf("Dial with pinned member: %v", err)
	}
	_ = c.Close()

	// A server whose key is not pinned must be refused — this is the
	// fake-server-harvests-the-JIT-config defense.
	addr2, _ := serveSSH(t, passwordConf())
	_, err = Dial(testCtx(t), addr2, cfg)
	if err == nil {
		t.Fatal("Dial accepted an unpinned host key")
	}
	if !strings.Contains(err.Error(), "pinned") {
		t.Errorf("err = %v, want the pin-set refusal", err)
	}
	if errors.Is(err, ErrAuthRejected) {
		t.Errorf("host-key refusal classified as auth rejection: %v", err)
	}
}

// WaitFor must ride out auth rejection, not abort: right after rotation,
// sshd may briefly reject the new key while its config flip settles.
func TestWaitForRetriesAuthRejection(t *testing.T) {
	signer := newTestSigner(t)
	var accept atomic.Bool
	keyOK := acceptKey(signer.PublicKey())
	addr, _ := serveSSH(t, &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !accept.Load() {
				return nil, errors.New("denied")
			}
			return keyOK(meta, key)
		},
	})
	go func() {
		time.Sleep(150 * time.Millisecond)
		accept.Store(true)
	}()
	ctx, cancel := bounded.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := WaitFor(ctx, addr, Config{User: "admin", Signer: signer, Timeout: time.Second}, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitFor did not survive transient auth rejection: %v", err)
	}
	_ = c.Close()
}

// TestWaitForNamesRejectionOnExpiry covers the same invariant the real-network
// half of TestWaitForRetriesAuthRejection used to (permanent rejection: the
// expiry error names it, in text, without leaking into the chain), but
// against waitFor directly with a zero-latency fake dial instead of a real
// SSH handshake. That real-handshake version raced two wall-clock budgets
// against each other (a 1s per-attempt Dial timeout against a 2s outer ctx) —
// reliable when the loopback dial+handshake was fast, but a real attempt on a
// contended CI runner (many concurrent bazel test actions on a small hosted
// macOS runner, observed CI failures unrelated to -race) could legitimately
// exceed its own 1s budget on every attempt, so lastErr never once held a
// clean rejection to report.
//
// Termination is driven from inside the fake dial (cancel() on the Nth
// call), not by racing a real deadline against real sleeps: a
// context-expires-after-some-wall-clock-duration version would just
// reintroduce a smaller copy of the same class of flake this test replaces
// — the goroutine could still be descheduled long enough to expire the
// context after fewer than N calls. The bounded.Context deadline here is
// only a backstop against this test hanging if cancel() were never reached;
// it is not what ends the loop.
func TestWaitForNamesRejectionOnExpiry(t *testing.T) {
	ctx, cancel := bounded.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	const wantCalls = 3
	calls := 0
	_, err := waitFor(ctx, "guest:22", time.Millisecond, func() (*Client, error) {
		calls++
		if calls == wantCalls {
			cancel()
		}
		return nil, ErrAuthRejected
	})
	if err == nil {
		t.Fatal("want waitFor expiry under permanent rejection")
	}
	if calls != wantCalls {
		t.Fatalf("want exactly %d dial attempts before expiry, got %d", wantCalls, calls)
	}
	if !strings.Contains(err.Error(), ErrAuthRejected.Error()) {
		t.Errorf("expiry error does not name the rejection: %v", err)
	}
	if errors.Is(err, ErrAuthRejected) {
		t.Errorf("lastErr leaked into the expiry chain (see WaitFor's %%v rationale): %v", err)
	}
}
