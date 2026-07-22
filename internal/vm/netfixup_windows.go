//go:build windows

package vm

import (
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// consoleDialRetryInterval paces retries while the console pipe server isn't
// listening yet -- a brief window right after Start returns, before HCS has
// finished wiring up the console device.
const consoleDialRetryInterval = 500 * time.Millisecond

// consolePromptPoll is how often fixupNetwork nudges the console with a
// blank line while waiting for the login prompt to appear -- many serial
// gettys print nothing at all until they see input.
const consolePromptPoll = 2 * time.Second

// fixupNetwork corrects a real image/Hyper-V incompatibility, confirmed
// against real hardware (issue #319): ghcr.io/cirruslabs/ubuntu-runner-amd64's
// baked-in netplan matches interface names "en*" -- correct for QEMU/VZ's
// PCI-enumerated virtio-net (darwin's own guests never hit this, which is
// why this fixup is windows-only), but wrong for Hyper-V's hv_netvsc, which
// always names the interface eth0 regardless of enumeration order. Without
// this, eth0 sits DOWN forever, DHCP never even starts, and WaitIP can never
// succeed -- confirmed via a from-scratch differencing-disk boot with zero
// prior state, so this isn't an artifact of a dirtied test image, and
// confirmed via a plain classic Hyper-V VM too, so it isn't specific to bare
// HCS compute systems either.
//
// This necessarily dials the console directly rather than SSH: the guest has
// no working network yet, which is exactly the problem being fixed. Once
// logged in, it writes a netplan drop-in matching by driver (the fix
// ADR-0026 already named as the production answer -- "the idiomatic
// Azure/Hyper-V pattern" -- once a real image demonstrated the need) and
// applies it, then verifies eth0 actually got an address before returning:
// a silent no-op here would just make a later WaitIP timeout far less
// diagnosable.
func fixupNetwork(ctx bounded.Context, systemID, sshUser, sshPassword string) error {
	conn, err := dialConsoleWithRetry(ctx, consolePipeName(systemID))
	if err != nil {
		return fmt.Errorf("dialing console: %w", err)
	}
	defer conn.Close()

	if err := consoleLogin(ctx, conn, sshUser, sshPassword); err != nil {
		return fmt.Errorf("console login: %w", err)
	}

	// One combined, single-line command: a heredoc doesn't survive the
	// console's line-ending handling reliably (confirmed the hard way), so
	// the drop-in is written via printf instead. Chained with && so a
	// failure at any stage short-circuits before the final ip addr check,
	// which is what the caller actually verifies against.
	const cmd = `printf 'network:\n  version: 2\n  ethernets:\n    eth0:\n      match:\n        driver: hv_netvsc\n      dhcp4: true\n' | sudo tee /etc/netplan/60-runny-hv-netvsc-fix.yaml >/dev/null && sudo netplan apply && sleep 5 && ip -4 -o addr show eth0`
	out, err := consoleRun(ctx, conn, cmd, 20*time.Second)
	if err != nil {
		return fmt.Errorf("applying network fixup: %w", err)
	}
	if !strings.Contains(out, "inet ") {
		return fmt.Errorf("network fixup did not bring up eth0 with an address; console output: %q", out)
	}
	return nil
}

// dialConsoleWithRetry retries the pipe dial within ctx: the console pipe
// server isn't guaranteed to be listening the instant Start returns.
func dialConsoleWithRetry(ctx bounded.Context, pipePath string) (net.Conn, error) {
	dialTimeout := 2 * time.Second
	for {
		conn, dialErr := winio.DialPipe(pipePath, &dialTimeout)
		if dialErr == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (last dial error: %v)", ctx.Err(), dialErr)
		case <-time.After(consoleDialRetryInterval):
		}
	}
}

// consoleLogin waits for a login prompt (nudging periodically, since a
// silent getty prints nothing until it sees input), then authenticates.
// Bounded by ctx throughout -- a wedged or never-arriving prompt surfaces as
// ctx.Err(), not a hang.
func consoleLogin(ctx bounded.Context, conn net.Conn, user, password string) error {
	var seen strings.Builder
	for !strings.Contains(seen.String(), "login:") {
		select {
		case <-ctx.Done():
			return fmt.Errorf("no login prompt: %w (last output: %q)", ctx.Err(), seen.String())
		default:
		}
		if err := consoleWrite(conn, "\r\n"); err != nil {
			return err
		}
		seen.WriteString(consoleDrain(conn, consolePromptPoll))
	}

	if err := consoleWrite(conn, user+"\r\n"); err != nil {
		return err
	}
	afterUser := consoleDrain(conn, 3*time.Second)
	if !strings.Contains(afterUser, "Password") {
		return fmt.Errorf("no password prompt after sending username; output: %q", afterUser)
	}

	if err := consoleWrite(conn, password+"\r\n"); err != nil {
		return err
	}
	afterPass := consoleDrain(conn, 4*time.Second)
	if strings.Contains(strings.ToLower(afterPass), "incorrect") {
		return fmt.Errorf("login rejected: %q", afterPass)
	}
	return nil
}

// consoleRun sends cmd (plus a trailing newline) and drains the response
// for window. The caller decides what the output means -- there's no exit
// code over a raw serial console, only text.
func consoleRun(ctx bounded.Context, conn net.Conn, cmd string, window time.Duration) (string, error) {
	if err := consoleWrite(conn, cmd+"\n"); err != nil {
		return "", err
	}
	out := consoleDrain(conn, window)
	select {
	case <-ctx.Done():
		return out, ctx.Err()
	default:
		return out, nil
	}
}

func consoleWrite(conn net.Conn, s string) error {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := io.WriteString(conn, s)
	return err
}

// consoleDrain reads whatever arrives within window and returns it, never
// blocking past the deadline -- a quiet console (nothing to read) is not an
// error, it's the common case between prompts.
func consoleDrain(conn net.Conn, window time.Duration) string {
	_ = conn.SetReadDeadline(time.Now().Add(window))
	buf := make([]byte, 8192)
	var got strings.Builder
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			got.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return got.String()
}
