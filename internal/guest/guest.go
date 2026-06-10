// Package guest adapts sshx sessions to the state machine's Guest interface:
// it knows how a cirruslabs guest stages the actions runner from the virtiofs
// cache share and launches run.sh with a JIT config.
package guest

import (
	"context"
	"fmt"
	"time"

	"github.com/bojanrajkovic/runny/internal/sshx"
	"github.com/bojanrajkovic/runny/internal/statemachine"
)

// Dialer implements statemachine.Dialer over sshx.
type Dialer struct {
	SSH sshx.Config
	// RetryInterval between connection attempts (default 2s).
	RetryInterval time.Duration
}

func (d Dialer) WaitFor(ctx context.Context, addr string) (statemachine.Guest, error) {
	interval := d.RetryInterval
	if interval == 0 {
		interval = 2 * time.Second
	}
	c, err := sshx.WaitFor(ctx, addr, d.SSH, interval)
	if err != nil {
		return nil, err
	}
	return &Guest{c: c}, nil
}

// Guest is one authenticated session into a booted runner VM.
type Guest struct {
	c *sshx.Client
}

// provisionScript stages the runner and execs run.sh. The cache share
// appears either at the automount path (macOS guests automount virtiofs
// shares tagged for it) or gets mounted explicitly by tag; handle both.
//
// The runner ALWAYS comes from our cache share, into a runny-owned dir —
// cirruslabs images ship a preinstalled ~/actions-runner whose version rots
// (a bundled v2.332.0 got "deprecated and cannot receive messages" from the
// broker), and JIT runners cannot self-update. Never trust the image's copy.
//
// Exit 78 (EX_CONFIG) = cache share missing the tarball — a host-side
// problem the post-mortem will show verbatim.
const provisionScript = `set -e
CACHE="/Volumes/My Shared Files"
if [ ! -d "$CACHE" ]; then
  sudo mkdir -p /Volumes/runny-cache 2>/dev/null || true
  sudo mount_virtiofs runny-cache /Volumes/runny-cache 2>/dev/null || true
  CACHE="/Volumes/runny-cache"
fi
TARBALL="$(ls "$CACHE"/actions-runner-osx-arm64-*.tar.gz 2>/dev/null | head -1)"
if [ -z "$TARBALL" ]; then echo "runny: no actions-runner tarball in cache share $CACHE" >&2; exit 78; fi
RUNNER_DIR="$HOME/runny-runner"
rm -rf "$RUNNER_DIR" && mkdir -p "$RUNNER_DIR" && cd "$RUNNER_DIR"
tar -xzf "$TARBALL"
exec ./run.sh --jitconfig '%s'
`

func (g *Guest) StartRunner(ctx context.Context, jit string) (statemachine.Proc, error) {
	p, err := g.c.Start(ctx, fmt.Sprintf(provisionScript, jit))
	if err != nil {
		return nil, fmt.Errorf("starting runner: %w", err)
	}
	return proc{p}, nil
}

// PullDiag fetches the tail of the runner's diagnostic logs — the
// post-mortem material TEARDOWN collects before destroying the guest.
func (g *Guest) PullDiag(ctx context.Context) ([]byte, error) {
	out, _, err := g.c.Output(ctx,
		`for f in $HOME/runny-runner/_diag/*.log; do echo "==> $f <=="; tail -c 32768 "$f"; done 2>/dev/null`)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (g *Guest) Close() error { return g.c.Close() }

// proc adapts sshx.Proc to statemachine.Proc.
type proc struct{ p *sshx.Proc }

func (p proc) Lines() <-chan string { return p.p.Lines }
func (p proc) Wait() (int, error)   { return p.p.Wait() }
func (p proc) Kill()                { p.p.Kill() }
