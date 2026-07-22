//go:build windows

package vm

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

func TestBoundedDeadline(t *testing.T) {
	t.Run("ctx deadline sooner than window", func(t *testing.T) {
		ctx, cancel := bounded.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		want, _ := ctx.Deadline()
		if got := boundedDeadline(ctx, time.Minute); !got.Equal(want) {
			t.Errorf("boundedDeadline = %v, want ctx's own deadline %v", got, want)
		}
	})

	t.Run("window sooner than ctx deadline", func(t *testing.T) {
		ctx, cancel := bounded.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		before := time.Now()
		got := boundedDeadline(ctx, 2*time.Second)
		if got.Before(before.Add(time.Second)) || got.After(before.Add(3*time.Second)) {
			t.Errorf("boundedDeadline = %v, want ~2s from %v", got, before)
		}
	})
}

func TestAppendCapped(t *testing.T) {
	acc := strings.Repeat("a", consoleSeenCap-2)
	acc = appendCapped(acc, "bbbb")
	if len(acc) != consoleSeenCap {
		t.Fatalf("len(acc) = %d, want %d", len(acc), consoleSeenCap)
	}
	if !strings.HasSuffix(acc, "bbbb") {
		t.Errorf("appendCapped dropped the newest bytes instead of the oldest: tail = %q", acc[len(acc)-8:])
	}
}

func TestConsoleWriteAndDrain(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		buf := make([]byte, 64)
		n, err := server.Read(buf)
		if err != nil {
			return
		}
		server.Write([]byte("echo: " + string(buf[:n])))
	}()

	ctx, cancel := bounded.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := consoleWrite(ctx, client, "hello\n"); err != nil {
		t.Fatalf("consoleWrite: %v", err)
	}
	out := consoleDrain(ctx, client, 2*time.Second, func(s string) bool { return strings.Contains(s, "echo:") })
	if !strings.Contains(out, "echo: hello") {
		t.Errorf("consoleDrain = %q, want it to contain the echoed input", out)
	}
}

// TestConsoleDrainEarlyExit is the regression test for the review finding
// that consoleDrain always blocked for its full window even once the
// expected marker had already arrived (issue #319 review).
func TestConsoleDrainEarlyExit(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go server.Write([]byte("eth0 inet 10.0.0.5/24 scope global"))

	ctx, cancel := bounded.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	out := consoleDrain(ctx, client, 20*time.Second, func(s string) bool { return strings.Contains(s, "inet ") })
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("consoleDrain took %v to return after an early match; want well under the 20s window", elapsed)
	}
	if !strings.Contains(out, "inet ") {
		t.Errorf("consoleDrain = %q, want the matched marker", out)
	}
}

// TestConsoleDrainNilMatchWaitsFullWindow covers the absence-check path
// (e.g. "no incorrect-password message showed up"): with match == nil,
// consoleDrain can only conclude "nothing else is coming" by waiting out
// the whole window, never early-exiting on silence.
func TestConsoleDrainNilMatchWaitsFullWindow(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	ctx, cancel := bounded.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	out := consoleDrain(ctx, client, 500*time.Millisecond, nil)
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Errorf("consoleDrain with a nil match returned after %v; want it to wait out the full window", elapsed)
	}
	if out != "" {
		t.Errorf("out = %q, want empty (nothing was written)", out)
	}
}

// fakeGetty emulates the console side of consoleLogin: a silent getty that
// only prints "login:" once it sees input, then a username and password
// prompt in sequence, matching consoleLogin's read/write choreography
// exactly (net.Pipe is unbuffered and synchronous, so mismatched ordering
// deadlocks rather than failing loudly).
func fakeGetty(t *testing.T, server net.Conn, password string) {
	t.Helper()
	buf := make([]byte, 256)

	if _, err := server.Read(buf); err != nil {
		return
	}
	if _, err := server.Write([]byte("login: ")); err != nil {
		t.Errorf("fakeGetty: writing login prompt: %v", err)
		return
	}

	n, err := server.Read(buf)
	if err != nil {
		t.Errorf("fakeGetty: reading username: %v", err)
		return
	}
	_ = n
	if _, err := server.Write([]byte("Password: ")); err != nil {
		t.Errorf("fakeGetty: writing password prompt: %v", err)
		return
	}

	n, err = server.Read(buf)
	if err != nil {
		t.Errorf("fakeGetty: reading password: %v", err)
		return
	}
	if strings.TrimSpace(string(buf[:n])) == password {
		server.Write([]byte("\r\nWelcome\r\n"))
	} else {
		server.Write([]byte("\r\nLogin incorrect\r\n"))
	}
}

func TestConsoleLoginSuccess(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go fakeGetty(t, server, "admin")

	ctx, cancel := bounded.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := consoleLogin(ctx, client, "admin", "admin"); err != nil {
		t.Fatalf("consoleLogin: %v", err)
	}
}

func TestConsoleLoginWrongPassword(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go fakeGetty(t, server, "admin")

	ctx, cancel := bounded.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := consoleLogin(ctx, client, "admin", "wrong")
	if err == nil || !strings.Contains(err.Error(), "login rejected") {
		t.Fatalf("consoleLogin = %v, want a login-rejected error", err)
	}
}

func TestConsoleLoginNoPrompt(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64)
		for {
			if _, err := server.Read(buf); err != nil {
				return
			}
		}
	}()

	ctx, cancel := bounded.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	err := consoleLogin(ctx, client, "admin", "admin")
	if err == nil || !strings.Contains(err.Error(), "no login prompt") {
		t.Errorf("consoleLogin = %v, want a no-login-prompt error", err)
	}
	server.Close()
	<-done
}

// TestConsoleRunEarlyExitOnMatch mirrors fixupNetwork's own netplan-apply
// call: the regression under test is the same fixed-20s-floor finding as
// TestConsoleDrainEarlyExit, exercised through consoleRun's actual call
// shape instead of consoleDrain directly.
func TestConsoleRunEarlyExitOnMatch(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		buf := make([]byte, 256)
		if _, err := server.Read(buf); err != nil {
			return
		}
		server.Write([]byte("eth0 inet 10.0.5.5/24 scope global\r\n"))
	}()

	ctx, cancel := bounded.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	out, err := consoleRun(ctx, client, "ip -4 -o addr show eth0", 20*time.Second, func(s string) bool { return strings.Contains(s, "inet ") })
	if err != nil {
		t.Fatalf("consoleRun: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("consoleRun took %v; want it to return promptly after the match instead of waiting the full 20s window", elapsed)
	}
	if !strings.Contains(out, "inet ") {
		t.Errorf("out = %q, want the matched marker", out)
	}
}
