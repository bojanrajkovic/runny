package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testServer is a minimal in-process SSH server: password auth, exec with
// canned behaviors. It exists so the deadline recipe is tested against real
// SSH wire behavior, not mocks.
func testServer(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	conf := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if meta.User() == "admin" && string(pass) == "admin" {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	conf.AddHostKey(signer)

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
	return ln.Addr().String()
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
		case payload.Cmd == "spew":
			// Far more output than the client's Lines buffer, then hang:
			// a chatty runner whose consumer has stopped draining.
			for i := range 500 {
				fmt.Fprintf(ch, "spew %d\n", i)
			}
			select {} // never exits; the client must Kill it
		case strings.HasPrefix(payload.Cmd, "hang"):
			select {} // never exits; the client must bound it
		default:
			exit(127)
		}
		return
	}
}

var testCfg = Config{User: "admin", Password: "admin", Timeout: 2 * time.Second}

func TestDialAndOutput(t *testing.T) {
	addr := testServer(t)
	c, err := Dial(t.Context(), addr, testCfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	out, code, err := c.Output(t.Context(), "hello")
	if err != nil || code != 0 {
		t.Fatalf("Output: %q, %d, %v", out, code, err)
	}
	if !strings.Contains(string(out), "hello world") {
		t.Errorf("out = %q", out)
	}
}

func TestOutputExitCode(t *testing.T) {
	c, err := Dial(t.Context(), testServer(t), testCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, code, err := c.Output(t.Context(), "fail")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if code != 3 || !strings.Contains(string(out), "boom") {
		t.Errorf("code=%d out=%q, want 3/boom", code, out)
	}
}

func TestDialWrongPassword(t *testing.T) {
	_, err := Dial(t.Context(), testServer(t), Config{User: "admin", Password: "nope", Timeout: 2 * time.Second})
	if err == nil {
		t.Fatal("want auth failure")
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
	_, err = Dial(t.Context(), ln.Addr().String(), Config{User: "admin", Password: "admin", Timeout: 500 * time.Millisecond})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want banner-hang failure")
	}
	if elapsed > 2*time.Second {
		t.Errorf("dial took %v; the deadline did not bound the banner exchange", elapsed)
	}
}

func TestOutputBoundedByContext(t *testing.T) {
	c, err := Dial(t.Context(), testServer(t), testCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
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
	c, err := Dial(t.Context(), testServer(t), testCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	p, err := c.Start(t.Context(), "stream")
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
	c, err := Dial(t.Context(), testServer(t), testCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	p, err := c.Start(ctx, "hang")
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
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	conf := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil },
	}
	conf.AddHostKey(signer)
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
	c, err := Dial(t.Context(), wedgedServer(t), cfg)
	if err != nil {
		t.Fatalf("Dial (handshake should succeed): %v", err)
	}
	defer c.Close()
	start := time.Now()
	_, _, err = c.Output(t.Context(), "hello")
	if err == nil {
		t.Fatal("want channel-open failure against a wedged guest")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Output took %v; the deadline did not bound the channel open", elapsed)
	}
}

func TestStartBoundedOnWedgedChannelOpen(t *testing.T) {
	cfg := Config{User: "admin", Password: "admin", Timeout: 500 * time.Millisecond}
	c, err := Dial(t.Context(), wedgedServer(t), cfg)
	if err != nil {
		t.Fatalf("Dial (handshake should succeed): %v", err)
	}
	defer c.Close()
	start := time.Now()
	_, err = c.Start(t.Context(), "hello")
	if err == nil {
		t.Fatal("want session failure against a wedged guest")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Start took %v; the deadline did not bound establishment", elapsed)
	}
}

// Kill must release readers blocked on a full Lines channel — without the
// proc's own done signal they parked forever once the FSM stopped draining
// (it stops at the completion marker), wedging Wait behind them.
func TestKillUnblocksWedgedReaders(t *testing.T) {
	c, err := Dial(t.Context(), testServer(t), testCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p, err := c.Start(t.Context(), "spew")
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
	c, err := Dial(t.Context(), testServer(t), testCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	base := runtime.NumGoroutine()
	for range 10 {
		p, err := c.Start(t.Context(), "stream")
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
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	c, err := WaitFor(ctx, addr, testCfg, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	_ = c.Close()

	// And expiry against a dead address.
	ctx2, cancel2 := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel2()
	_, err = WaitFor(ctx2, "127.0.0.1:1", Config{User: "a", Password: "b", Timeout: 100 * time.Millisecond}, 50*time.Millisecond)
	if err == nil {
		t.Fatal("want WaitFor expiry on dead address")
	}
}
