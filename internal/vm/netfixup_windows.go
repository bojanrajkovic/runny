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

// consoleDialTimeout bounds each individual pipe-dial attempt inside
// dialConsoleWithRetry.
const consoleDialTimeout = 2 * time.Second

// consoleDialRetryInterval paces retries while the console pipe server isn't
// listening yet -- a brief window right after Start returns, before HCS has
// finished wiring up the console device.
const consoleDialRetryInterval = 500 * time.Millisecond

// consolePromptPoll is how often fixupNetwork nudges the console with a
// blank line while waiting for the login prompt to appear -- many serial
// gettys print nothing at all until they see input.
const consolePromptPoll = 2 * time.Second

// consoleReadPoll bounds each individual Read inside consoleDrain: short
// enough that a positive match (e.g. fixupNetwork's "inet " check) is
// detected within one poll interval of arriving, instead of only once the
// whole window elapses.
const consoleReadPoll = 500 * time.Millisecond

// consoleSeenCap bounds accumulated console output kept for marker matching.
// A wedged or endlessly chatty console that never produces the expected
// marker must not grow this without bound for the life of ctx (ADR-0011's
// no-unbounded-operations invariant); every marker checked here ("login:",
// "Password", "incorrect", "inet ") is short, so only the most recent bytes
// can ever matter -- older output is dropped.
const consoleSeenCap = 4096

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
// HCS compute systems either. hcsMachine.WaitIP calls this once, as a
// fallback, after giving the guest a grace period to self-configure -- so a
// future image that doesn't need it never pays this cost.
//
// This necessarily dials the console directly rather than SSH: the guest has
// no working network yet, which is exactly the problem being fixed. Once
// logged in, it writes a netplan drop-in matching by driver -- the idiomatic
// Azure/Hyper-V pattern -- and applies it, then verifies eth0 actually got an
// address before returning: a silent no-op here would just make a later
// WaitIP timeout far less diagnosable.
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
	out, err := consoleRun(ctx, conn, cmd, 20*time.Second, func(s string) bool { return strings.Contains(s, "inet ") })
	if err != nil {
		return fmt.Errorf("applying network fixup: %w", err)
	}
	if !strings.Contains(out, "inet ") {
		return fmt.Errorf("network fixup did not bring up eth0 with an address; console output: %q", out)
	}
	return nil
}

// dialConsoleWithRetry retries the pipe dial within ctx: the console pipe
// server isn't guaranteed to be listening the instant Start returns. Each
// attempt's own timeout is capped by ctx's remaining budget so a slow or
// absent listener right at ctx's deadline can't overrun it by a further
// consoleDialTimeout.
func dialConsoleWithRetry(ctx bounded.Context, pipePath string) (net.Conn, error) {
	for {
		dialTimeout := consoleDialTimeout
		if d, ok := ctx.Deadline(); ok {
			if remaining := time.Until(d); remaining < dialTimeout {
				dialTimeout = remaining
			}
		}
		if dialTimeout <= 0 {
			return nil, fmt.Errorf("dialing console: %w", ctx.Err())
		}
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
	var seen string
	for !strings.Contains(seen, "login:") {
		select {
		case <-ctx.Done():
			return fmt.Errorf("no login prompt: %w (last output: %q)", ctx.Err(), seen)
		default:
		}
		if err := consoleWrite(ctx, conn, "\r\n"); err != nil {
			return err
		}
		// nil match: "login:" may straddle two separate drain calls (each
		// starts its own read buffer), so only the externally-accumulated,
		// capped seen is ever checked for it -- never a single call's own
		// chunk in isolation.
		seen = appendCapped(seen, consoleDrain(ctx, conn, consolePromptPoll, nil))
	}

	if err := consoleWrite(ctx, conn, user+"\r\n"); err != nil {
		return err
	}
	afterUser := consoleDrain(ctx, conn, 3*time.Second, func(s string) bool { return strings.Contains(s, "Password") })
	if !strings.Contains(afterUser, "Password") {
		return fmt.Errorf("no password prompt after sending username; output: %q", afterUser)
	}

	if err := consoleWrite(ctx, conn, password+"\r\n"); err != nil {
		return err
	}
	afterPass := consoleDrain(ctx, conn, 4*time.Second, func(s string) bool {
		return strings.Contains(strings.ToLower(s), "incorrect")
	})
	if strings.Contains(strings.ToLower(afterPass), "incorrect") {
		// Never embed afterPass here: login(1)/PAM disable tty echo before
		// this write, so the password itself shouldn't be in it, but
		// whatever else the console printed still shouldn't end up verbatim
		// in a persisted cycle failure record.
		return fmt.Errorf("login rejected")
	}
	return nil
}

// consoleRun sends cmd (plus a trailing newline) and drains the response
// until match reports true or window elapses, whichever is sooner. The
// caller decides what the output means -- there's no exit code over a raw
// serial console, only text.
func consoleRun(ctx bounded.Context, conn net.Conn, cmd string, window time.Duration, match func(string) bool) (string, error) {
	if err := consoleWrite(ctx, conn, cmd+"\n"); err != nil {
		return "", err
	}
	out := consoleDrain(ctx, conn, window, match)
	select {
	case <-ctx.Done():
		return out, ctx.Err()
	default:
		return out, nil
	}
}

// consoleWrite's deadline is bounded by ctx, not just its own fixed window --
// see boundedDeadline.
func consoleWrite(ctx bounded.Context, conn net.Conn, s string) error {
	_ = conn.SetWriteDeadline(boundedDeadline(ctx, 5*time.Second))
	_, err := io.WriteString(conn, s)
	return err
}

// consoleDrain reads from conn in short increments until match reports true
// on the accumulated output, or the window/ctx deadline (whichever is
// sooner) arrives -- never blocking past either. match == nil always waits
// the full window: used wherever what's being checked is an absence (e.g.
// "no incorrect-password message showed up"), which can only be decided
// once nothing more is coming. A quiet console (nothing to read) is not an
// error, it's the common case between prompts.
func consoleDrain(ctx bounded.Context, conn net.Conn, window time.Duration, match func(string) bool) string {
	deadline := boundedDeadline(ctx, window)
	buf := make([]byte, 8192)
	var got string
	for {
		readDeadline := time.Now().Add(consoleReadPoll)
		if readDeadline.After(deadline) {
			readDeadline = deadline
		}
		_ = conn.SetReadDeadline(readDeadline)
		n, err := conn.Read(buf)
		if n > 0 {
			got = appendCapped(got, string(buf[:n]))
			if match != nil && match(got) {
				return got
			}
		}
		if !time.Now().Before(deadline) {
			return got
		}
		if err != nil {
			if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
				return got // real read error (closed pipe, reset, etc.) -- retrying won't help
			}
		}
	}
}

// boundedDeadline is the earlier of ctx's own deadline and now+window: every
// console read/write must stay inside ctx's real bound (ADR-0011), not just
// window's fixed duration -- window is only the caller's local ceiling.
func boundedDeadline(ctx bounded.Context, window time.Duration) time.Time {
	deadline := time.Now().Add(window)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		return d
	}
	return deadline
}

// appendCapped appends add to acc, dropping the oldest bytes once the result
// exceeds consoleSeenCap.
func appendCapped(acc, add string) string {
	acc += add
	if len(acc) > consoleSeenCap {
		acc = acc[len(acc)-consoleSeenCap:]
	}
	return acc
}
