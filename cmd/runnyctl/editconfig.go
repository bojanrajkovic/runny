package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/testconfig"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// configSkeleton seeds a from-scratch edit-config session: the schema modeline
// (for editor autocomplete/validation) plus an empty pools list to fill in.
const configSkeleton = `# yaml-language-server: $schema=https://raw.githubusercontent.com/bojanrajkovic/runny/main/tools/configschema/config.schema.json
pools: []
`

// configBytes reads the resolved home's config.yaml. The daemon is asked
// first and on a system home is the only thing that can answer: the operator's
// ACL entry stops at the home DIRECTORY, so the operator cannot open the file
// itself. A direct read is the fallback for a daemon that is down or predates
// GetConfig — which is the ordinary path for a per-user home, whose owner is
// the operator anyway.
//
// os.ErrNotExist means, and only means, "there is no config". Callers seed a
// blank skeleton from it, so a read that FAILED must never arrive wearing that
// error: applying a skeleton over a live fleet's config destroys it. When both
// attempts fail, both reasons are reported — the daemon being down and the
// file being unreadable have different fixes.
func (c *ctl) configBytes(ctx context.Context, path string) ([]byte, error) {
	rctx, cancel := context.WithTimeout(ctx, probeCallTimeout)
	defer cancel()
	resp, rpcErr := c.client.GetConfig(rctx, &runnyv1.GetConfigRequest{})
	switch status.Code(rpcErr) {
	case codes.OK:
		return resp.Content, nil
	case codes.NotFound:
		return nil, fmt.Errorf("%s: %w", path, os.ErrNotExist)
	}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		return b, nil
	case errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	return nil, fmt.Errorf("the daemon could not be asked for its config (%v) and reading %s directly failed: %w",
		rpcErr, path, err)
}

// editConfig is `runnyctl edit-config` — visudo semantics for the resolved
// home's config.yaml: edit a temp copy, validate it with `runnyd -test-config`
// (reopening the editor on the operator's edits on failure, never discarding
// them), swap it in only once it validates, then reload the running daemon (or
// report it applies on next start). Works for both the per-user and system
// home, with no sudo either way: on a system home the daemon reads and writes
// the file on the operator's behalf, so the operator needs no access to it.
func (c *ctl) editConfig(ctx context.Context) error {
	dir, err := home.ResolveClient()
	if err != nil {
		return err
	}
	configPath := dir.ConfigPath()

	runnyd, err := resolveRunnyd()
	if err != nil {
		return err
	}

	seed, err := c.configBytes(ctx, configPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		seed = []byte(configSkeleton)
	}

	// The scratch copy lives in the operator's own temp dir, not the daemon's
	// home: the edit round-trips through RPCs, so there is no reason to require
	// (or exercise) write access to the home for the editing itself. Config
	// validation is not path-sensitive — home.ParseConfig takes the path for
	// error messages only, and private_key_path is read verbatim rather than
	// resolved against the config's directory — so a copy elsewhere validates
	// identically.
	tmp, err := os.CreateTemp("", "runny-config-*.yaml")
	if err != nil {
		return fmt.Errorf("creating a temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Always removed now that the apply is a copy rather than a rename: the
	// scratch file holds a config the operator authored, and it outlives the
	// command otherwise.
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(seed); err != nil {
		tmp.Close()
		return fmt.Errorf("seeding the temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("seeding the temp file: %w", err)
	}

	var after []byte
	for {
		if err := openEditor(tmpPath); err != nil {
			return err
		}
		after, err = os.ReadFile(tmpPath)
		if err != nil {
			return fmt.Errorf("reading the edited config: %w", err)
		}
		if bytes.Equal(after, seed) {
			fmt.Fprintln(c.out, "no changes")
			return nil
		}
		v, err := testconfig.RunTestConfig(ctx, runnyd, tmpPath)
		if err != nil {
			return err
		}
		for _, w := range v.Warnings {
			fmt.Fprintf(c.err, "warning: %s\n", w.Message)
		}
		for _, e := range v.Errors {
			fmt.Fprintf(c.err, "error: %s\n", e)
		}
		switch v.Status {
		case home.VerdictOK:
		case home.VerdictWarn:
			if !confirmYN("apply despite the warning(s) above?") {
				fmt.Fprintln(c.err, "reopening the editor…")
				continue
			}
		default:
			// home.VerdictError, or any status this runnyctl doesn't recognize — fail
			// closed like decideUpgrade does for the same contract (upgradedaemon.go):
			// an unrecognized status must never be treated as an implicit ok.
			fmt.Fprintln(c.err, "fix the error(s) above — reopening the editor…")
			continue
		}
		break
	}

	sum, err := c.applyConfig(ctx, configPath, after)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "config saved (sha256 %s)\n", shortHex(sum))

	pctx, cancel := context.WithTimeout(ctx, probeCallTimeout)
	_, statusErr := c.client.GetStatus(pctx, &runnyv1.GetStatusRequest{})
	cancel()
	if statusErr != nil {
		fmt.Fprintln(c.out, "daemon not running; this config applies on next start")
		return nil
	}
	return c.reloadWait(ctx, "runnyctl edit-config", c.plainReload, defaultFollowOpts(90*time.Second, 0))
}

// applyConfig persists the edited bytes and returns the hex SHA-256 of what
// landed. The daemon writes it when it can, which is what keeps config.yaml
// owned by the daemon rather than by whichever operator edited last — an
// operator cannot chown a file to the service account, so a rename from the
// client is a one-way door on ownership.
//
// The direct write is the fallback for a daemon that is down or predates
// SetConfig. On a system home it will fail for anyone but the file's owner,
// and that is the intended shape: recovery for a stopped system daemon is
// `sudo runnyctl install-daemon --config`, not a hand-write into its home.
//
// The digest is taken from the daemon's own answer, so what is printed is what
// the daemon persisted rather than what the client believes it sent — the same
// hash then appears as ReloadResponse.config_sha256 when the reload picks it up.
func (c *ctl) applyConfig(ctx context.Context, configPath string, content []byte) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, probeCallTimeout)
	defer cancel()
	switch resp, err := c.client.SetConfig(rctx, &runnyv1.SetConfigRequest{Content: content}); {
	case err == nil:
		return resp.ConfigSha256, nil
	case status.Code(err) != codes.Unavailable && status.Code(err) != codes.Unimplemented:
		// The daemon answered and refused. Writing behind its back would apply
		// an edit it rejected.
		return "", fmt.Errorf("applying the edited config: %w", err)
	}
	if err := home.AtomicWrite(configPath, content); err != nil {
		return "", fmt.Errorf("applying the edited config: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(content)), nil
}

// openEditor runs $VISUAL, else $EDITOR, else vi on path, connected to the
// current terminal. The convention (VISUAL taking precedence for full-screen
// editors) is the traditional Unix one; the editor string is split on
// whitespace — covers "vim"/"nano" and "code --wait", not an editor whose own
// path contains a space (rare enough not to special-case: ponytail).
func openEditor(path string) error {
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	argv := strings.Fields(editor)
	if len(argv) == 0 {
		argv = []string{"vi"}
	}
	cmd := exec.Command(argv[0], append(argv[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", strings.Join(argv, " "), err)
	}
	return nil
}

// stdinReader is shared across every confirmYN call in a session. A fresh
// bufio.Reader per call would over-read a piped stdin that already has
// multiple answer lines buffered at the OS level (e.g. a script piping
// "n\ny\n" to decline once then accept on retry) — the first Reader's
// internal buffer could swallow the second line before ever returning it,
// leaving the next confirmYN blocked against an already-drained pipe.
var stdinReader = bufio.NewReader(os.Stdin)

// confirmYN prompts prompt + " [y/N] " on stderr and reads a line from stdin;
// anything but a leading y/Y is "no" — the safe default for an unattended or
// piped invocation.
func confirmYN(prompt string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	line, _ := stdinReader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}
