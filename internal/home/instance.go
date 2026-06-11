package home

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// InstanceIDPath holds the persisted per-install runner-name prefix.
func (d Dir) InstanceIDPath() string { return filepath.Join(string(d), "instance-id") }

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// maxRunnerName is GitHub's runner-name length cap (generate-jitconfig
// rejects longer names with a non-retryable 422).
const maxRunnerName = 64

// ValidateRunnerNames rejects any pool whose worst-case assembled runner
// name — <prefix>-<pool>-<count>-<cycle8> — would exceed GitHub's limit.
// Checked at startup with the real prefix (covers a hand-edited
// instance-id too): without this, the 422 surfaces as a permanent
// MINT_JIT backoff loop with no hint that the NAME is the problem.
func ValidateRunnerNames(prefix string, pools []PoolConfig) error {
	var errs []error
	for _, p := range pools {
		worst := fmt.Sprintf("%s-%s-%d-%s", prefix, p.Name, p.Count, "00000000")
		if over := len(worst) - maxRunnerName; over > 0 {
			errs = append(errs, fmt.Errorf(
				"pool %q: runner names like %q exceed GitHub's %d-char limit by %d — shorten the pool name (the %q prefix is derived from the hostname)",
				p.Name, worst, maxRunnerName, over, prefix,
			))
		}
	}
	return errors.Join(errs...)
}

// slugHostname reduces a hostname to a GitHub-runner-name-safe token: the
// short name (cut at the first dot), lowercased, every run of non-alphanumeric
// collapsed to a single dash, trimmed, and length-capped.
func slugHostname(h string) string {
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	h = slugNonAlnum.ReplaceAllString(strings.ToLower(h), "-")
	h = strings.Trim(h, "-")
	if len(h) > 24 {
		h = strings.Trim(h[:24], "-")
	}
	if h == "" {
		h = "runny"
	}
	return h
}

// InstancePrefix returns this install's runner-name prefix,
// <slug(hostname)>-<rand8>, generating and persisting it on first use.
//
// It is the daemon's ownership namespace, not a cosmetic label: every runner
// this install registers carries it, and the startup sweep deletes offline
// registrations by matching it (sweepRegistrations). That is why it is
// derived, not configured — a mistyped prefix would either orphan runners
// beyond the sweep's reach or, worse, match another host's — and why it is
// persisted rather than regenerated per process: a crash-and-restart must
// keep the same namespace or the previous cycles' runners become unsweepable.
func (d Dir) InstancePrefix() (string, error) {
	p := d.InstanceIDPath()
	switch b, err := os.ReadFile(p); {
	case err == nil:
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, nil
		}
		// Empty/corrupt file: fall through and regenerate.
	case !os.IsNotExist(err):
		return "", fmt.Errorf("reading instance id: %w", err)
	}
	host, err := os.Hostname()
	if err != nil {
		host = "runny"
	}
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", fmt.Errorf("generating instance id: %w", err)
	}
	id := slugHostname(host) + "-" + hex.EncodeToString(rnd[:])
	if err := os.WriteFile(p, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("writing instance id: %w", err)
	}
	return id, nil
}
