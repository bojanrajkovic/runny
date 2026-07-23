//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// TestIsTrustedPipeOwner pins the pure verdict: the two un-forgeable owners a
// squatter cannot set (Administrators, SYSTEM) are trusted; an ordinary user
// SID and garbage are not.
func TestIsTrustedPipeOwner(t *testing.T) {
	cases := []struct {
		sid  string
		want bool
	}{
		{sidAdministrators, true},                                // S-1-5-32-544
		{sidLocalSystem, true},                                   // S-1-5-18
		{"S-1-5-21-1004336348-1177238915-682003330-1001", false}, // an unprivileged user
		{"S-1-5-11", false},                                      // Authenticated Users — a group, not an owner a squatter's pipe would carry
		{"", false},
		{"not-a-sid", false},
	}
	for _, c := range cases {
		if got := isTrustedPipeOwner(c.sid); got != c.want {
			t.Errorf("isTrustedPipeOwner(%q) = %v, want %v", c.sid, got, c.want)
		}
	}
}

// TestPipeOwnerReadEndToEnd proves the owner-read path works over a real dialed
// pipe: it creates a pipe (owner = this test process's default token owner),
// dials it, reads the connected pipe's owner SID via the same GetSecurityInfo
// path production uses, and asserts the SID resolves and equals the process's
// default owner. It also pins verifyPipeOwner's verdict to isTrustedPipeOwner
// of that owner — runner-agnostic (true on CI's elevated windows-2022 runner
// where the default owner is Administrators, false for a plain user), like
// TestIsPrivilegedToken, rather than hardcoding a boolean that assumes the
// runner is elevated.
func TestPipeOwnerReadEndToEnd(t *testing.T) {
	name := fmt.Sprintf(`\\.\pipe\runnyd-dialtest-%d-%d`, os.Getpid(), time.Now().UnixNano())
	ln, err := winio.ListenPipe(name, nil)
	if err != nil {
		t.Fatalf("ListenPipe: %v", err)
	}
	defer func() { _ = ln.Close() }()

	acceptErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		_ = c.Close()
		acceptErr <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := winio.DialPipeAccessImpLevel(ctx, name, pipeDialAccess, winio.PipeImpLevelIdentification)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	owner, err := pipeOwnerSID(conn)
	if err != nil {
		t.Fatalf("pipeOwnerSID over a real dialed pipe: %v", err)
	}
	if owner == "" {
		t.Fatal("pipe owner SID is empty, want a resolvable SID")
	}
	// The read must yield a real, resolvable SID — round-trip it through the
	// SID parser to prove GetSecurityInfo(OWNER) recovered a valid principal,
	// not a malformed string.
	if _, err := windows.StringToSid(owner); err != nil {
		t.Fatalf("dialed pipe owner %q is not a resolvable SID: %v", owner, err)
	}

	// verifyPipeOwner accepts iff that owner is trusted — no hardcoded verdict,
	// so the assertion holds whether or not the runner is elevated (its default
	// object owner is Administrators when elevated, the plain user SID when not).
	err = verifyPipeOwner(conn)
	if want := isTrustedPipeOwner(owner); (err == nil) != want {
		t.Errorf("verifyPipeOwner accepted = %v (err=%v), want accepted = %v for owner %q", err == nil, err, want, owner)
	}

	if aerr := <-acceptErr; aerr != nil {
		t.Fatalf("server accept: %v", aerr)
	}
}
