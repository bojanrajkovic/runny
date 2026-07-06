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

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/sysdaemon"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// configSkeleton seeds a from-scratch edit-config session: the schema modeline
// (for editor autocomplete/validation) plus an empty pools list to fill in.
const configSkeleton = `# yaml-language-server: $schema=https://raw.githubusercontent.com/bojanrajkovic/runny/main/tools/configschema/config.schema.json
pools: []
`

// editConfig is `runnyctl edit-config` — visudo semantics for the resolved
// home's config.yaml: edit a temp copy, validate it with `runnyd -test-config`
// (reopening the editor on the operator's edits on failure, never discarding
// them), atomically swap it in only once it validates, then reload the running
// daemon (or report it applies on next start). Works for both the per-user and
// system home — the operator reaches the system home via its inheriting ACL,
// so no sudo is needed either way.
func (c *ctl) editConfig(ctx context.Context) error {
	dir, err := home.ResolveClient()
	if err != nil {
		return err
	}
	configPath := dir.ConfigPath()

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating runnyctl: %w", err)
	}
	runnyd := sysdaemon.ResolveRunnydPath(exe)
	if _, err := os.Stat(runnyd); err != nil {
		return fmt.Errorf("runnyd not found next to runnyctl at %s: %w", runnyd, err)
	}

	seed, err := os.ReadFile(configPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("reading %s: %w", configPath, err)
		}
		seed = []byte(configSkeleton)
	}

	if err := os.MkdirAll(dir.String(), 0o700); err != nil {
		return fmt.Errorf("preparing %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir.String(), ".config.yaml.*.tmp")
	if err != nil {
		return fmt.Errorf("creating a temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed onto configPath below
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
		v, err := runConfigGate(runnyd, tmpPath)
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
		case verdictOK:
		case verdictWarn:
			if !confirmYN("apply despite the warning(s) above?") {
				fmt.Fprintln(c.err, "reopening the editor…")
				continue
			}
		default:
			// verdictError, or any status this runnyctl doesn't recognize — fail
			// closed like decideUpgrade does for the same contract (upgradedaemon.go):
			// an unrecognized status must never be treated as an implicit ok.
			fmt.Fprintln(c.err, "fix the error(s) above — reopening the editor…")
			continue
		}
		break
	}

	if err := os.Rename(tmpPath, configPath); err != nil {
		return fmt.Errorf("applying the edited config: %w", err)
	}
	fmt.Fprintf(c.out, "config saved (sha256 %.12x)\n", sha256.Sum256(after))

	pctx, cancel := context.WithTimeout(ctx, probeCallTimeout)
	_, statusErr := c.client.GetStatus(pctx, &runnyv1.GetStatusRequest{})
	cancel()
	if statusErr != nil {
		fmt.Fprintln(c.out, "daemon not running; this config applies on next start")
		return nil
	}
	return c.reloadWait(ctx, "runnyctl edit-config",
		func(ctx context.Context, req *runnyv1.ReloadRequest) (*runnyv1.ReloadResponse, error) {
			return c.client.Reload(ctx, req)
		},
		defaultFollowOpts(90*time.Second, 0))
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
