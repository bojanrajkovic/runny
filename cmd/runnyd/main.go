// runnyd is the runner daemon: one crash-only state machine per runner slot,
// serving the runny.v1 control surface over a unix socket. Restart is a cold
// start by design (ADR-0004): validate, sweep, run.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/cycle"
	"github.com/bojanrajkovic/runny/internal/github"
	"github.com/bojanrajkovic/runny/internal/guest"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/images"
	"github.com/bojanrajkovic/runny/internal/logring"
	"github.com/bojanrajkovic/runny/internal/oci"
	"github.com/bojanrajkovic/runny/internal/socket"
	"github.com/bojanrajkovic/runny/internal/sshx"
	"github.com/bojanrajkovic/runny/internal/statemachine"
	"github.com/bojanrajkovic/runny/internal/tart"
)

var version = "dev"

// macOSGuestCap: Virtualization.framework hard-caps concurrent macOS guests.
// The predecessor accepted configs over the cap and churned forever; we
// refuse to start (pillar 7).
const macOSGuestCap = 2

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "runnyd:", err)
		os.Exit(1)
	}
}

func run() error {
	homeFlag := flag.String("home", "", "runny home dir (default $RUNNY_HOME or ~/.runny)")
	configFlag := flag.String("config", "", "config path (default <home>/config.yaml)")
	checkOnly := flag.Bool("doctor", false, "run validation checks and exit")
	flag.Parse()

	dir, err := home.Resolve(*homeFlag)
	if err != nil {
		return err
	}
	if err := dir.Ensure(); err != nil {
		return err
	}
	configPath := *configFlag
	if configPath == "" {
		configPath = dir.ConfigPath()
	}
	cfg, err := home.LoadConfig(configPath)
	if err != nil {
		return err
	}

	// Take the single-instance lock before touching the shared log file: a
	// losing second instance must not interleave its startup lines into the
	// winner's structured log. -doctor stays lock-free (read-only, useful
	// against a live daemon).
	if !*checkOnly {
		lock, err := acquireLock(dir.LockPath())
		if err != nil {
			return err
		}
		defer lock.Close()
	}

	// Logging: size-capped file sink + ring buffer, both structured.
	logFile, err := openRotatingFile(dir.LogFile(), logFileCap)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}
	defer logFile.Close()
	ring := logring.NewRing(4096)
	logger := slog.New(logring.NewHandler(logFile, slog.LevelDebug, ring))
	slog.SetDefault(logger)
	logger.Info("runnyd starting", "version", version, "home", dir.String())

	// One client per distinct registration target; App credentials shared.
	ghCfg := github.Config{
		AppID:          cfg.GitHub.AppID,
		PrivateKeyPath: cfg.GitHub.PrivateKeyPath,
		APIBase:        cfg.GitHub.APIBase,
	}
	clients := map[home.TargetConfig]*github.Client{}
	for _, p := range cfg.Pools {
		if _, ok := clients[p.Target]; ok {
			continue
		}
		c, err := github.New(ghCfg, github.Target(p.Target))
		if err != nil {
			return err
		}
		clients[p.Target] = c
	}

	doctor := makeDoctor(dir, cfg, clients)
	if *checkOnly {
		return runDoctor(doctor) // read-only: runs fine alongside a live daemon
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Startup validation: fail loudly now, not silently later.
	if failed := failedChecks(doctor(ctx)); len(failed) > 0 {
		for _, c := range failed {
			logger.Error("startup check failed", "check", c.Name, "detail", c.Detail)
		}
		return fmt.Errorf("%d startup check(s) failed (run runnyd -doctor for detail)", len(failed))
	}

	// Sweep: cold start owns the world (ADR-0004). Clones first, then any
	// offline registrations carrying our prefix, on every target.
	if err := os.RemoveAll(dir.VMsDir()); err != nil {
		return fmt.Errorf("sweeping vms dir: %w", err)
	}
	if err := dir.Ensure(); err != nil {
		return err
	}
	for _, c := range clients {
		sweepRegistrations(ctx, logger, c, cfg.NamePrefix)
	}

	// No startup tarball priming: runner tarballs are ensured inside each
	// cycle's ENSURE_IMAGE, where downloads get stall budgets, retries, and
	// why-visibility. Startup network work blocked the socket and once
	// dead-stalled silently — never again.
	var slots []*statemachine.Slot
	for _, p := range cfg.Pools {
		ref, err := oci.ParseRef(p.Image)
		if err != nil {
			return fmt.Errorf("pool %s: %w", p.Name, err)
		}
		gh, osName := clients[p.Target], p.OS
		deps := statemachine.Deps{
			Home:   dir,
			Config: cfg,
			Pool:   p,
			VM:     vmManager(),
			Images: &images.Ensurer{
				Home: dir,
				Ref:  ref,
				Runner: func(c bounded.Context) (string, string, string, error) {
					return gh.RunnerDownload(c, osName)
				},
				StallBudget:   cfg.Deadlines.PullStall.D(),
				ResolveBudget: cfg.Deadlines.Resolve.D(),
				Log:           logger,
			},
			Clone: func(src tart.Bundle, dst string) error {
				_, err := tart.Clone(src, tart.Bundle(dst))
				return err
			},
			GitHub: clients[p.Target],
			Dial: guest.Dialer{SSH: sshx.Config{
				User:     p.SSHUser,
				Password: p.SSHPassword,
				Timeout:  p.SSHTimeout.D(),
			}},
			Log: logger,
		}
		for i := 1; i <= p.Count; i++ {
			slots = append(slots, statemachine.NewSlot(fmt.Sprintf("%s-%d", p.Name, i), deps))
		}
	}

	srv := socket.NewServer(slots, ring,
		func(slot string) cycle.Store { return cycle.Store{SlotDir: dir.SlotCyclesDir(slot)} },
		doctor, version)

	// Wedge escalation (ADR-0012): a guest that survives force-stop can only
	// be reclaimed by process exit (it lives in-process). The wedged slot has
	// parked itself. Drain the rest of the fleet to a stable idle — pause
	// holds each slot in BACKOFF after its current cycle (a running job
	// finishes first), recycle ends LISTENING without waiting out max-idle —
	// then exit for a launchd cold start. Exiting only from stable states
	// (parked, or paused in BACKOFF, which cannot start a job) closes the
	// scan-then-exit race that could kill a job starting mid-scan.
	var drainStarted, wedgeExit atomic.Bool
	checkWedge := func(st statemachine.Status) {
		if !st.Wedged && !drainStarted.Load() {
			return // nothing wedged; stay off the hot status path
		}
		if drainStarted.CompareAndSwap(false, true) {
			logger.Error("slot wedged: draining remaining slots to idle, then restarting to release the guest")
			for _, s := range slots {
				s.Command(statemachine.Command{Kind: statemachine.CmdPause})
				s.Command(statemachine.Command{Kind: statemachine.CmdRecycle, Reason: "draining for wedge restart"})
			}
		}
		for _, s := range slots {
			sst := s.Status()
			if !(sst.Wedged || (sst.Paused && sst.State == statemachine.StateBackoff)) {
				return // still draining; a later status change re-evaluates
			}
		}
		if wedgeExit.CompareAndSwap(false, true) {
			logger.Error("fleet idle with a wedged guest; exiting for a cold start")
			stop()
		}
	}
	for _, s := range slots {
		s.OnChange(checkWedge)
	}

	var wg sync.WaitGroup
	for _, s := range slots {
		wg.Go(func() { s.Run(ctx) })
	}
	logger.Info("slots running", "count", len(slots), "socket", dir.SocketPath())

	err = srv.Serve(ctx, dir.SocketPath())
	wg.Wait()
	logger.Info("runnyd stopped")
	if wedgeExit.Load() && err == nil {
		// Non-zero exit so launchd (KeepAlive) restarts us; the cold start
		// sweeps the vms dir and reclaims the leaked guest.
		err = errors.New("restarting to release a wedged guest: a VM survived force-stop (see the slot's cycle record)")
	}
	return err
}

// failedChecks filters to failures.
func failedChecks(checks []socket.DoctorCheck) []socket.DoctorCheck {
	var out []socket.DoctorCheck
	for _, c := range checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

func runDoctor(doctor func(context.Context) []socket.DoctorCheck) error {
	checks := doctor(context.Background()) // each network check self-bounds (checkBudget)
	bad := 0
	for _, c := range checks {
		mark := "ok"
		if !c.OK {
			mark, bad = "FAIL", bad+1
		}
		fmt.Printf("%-28s %-4s %s\n", c.Name, mark, c.Detail)
	}
	if bad > 0 {
		return fmt.Errorf("%d check(s) failed", bad)
	}
	return nil
}

// checkBudget bounds each network-touching doctor check individually, no
// matter what context the caller supplies: the Doctor RPC must not rely on
// runnyctl for a bound, and the startup pass runs under the un-deadlined
// signal context — a silent registry must not hang the daemon before the
// socket even exists. Per-check rather than per-suite so one slow target
// cannot starve the remaining checks into false failures.
const checkBudget = 30 * time.Second

// makeDoctor builds the validation suite used at startup and by the Doctor
// RPC: every predecessor failure mode that was checkable but unchecked.
func makeDoctor(dir home.Dir, cfg *home.Config, clients map[home.TargetConfig]*github.Client) func(context.Context) []socket.DoctorCheck {
	return func(ctx context.Context) []socket.DoctorCheck {
		var checks []socket.DoctorCheck
		add := func(name string, ok bool, detail string) {
			checks = append(checks, socket.DoctorCheck{Name: name, OK: ok, Detail: detail})
		}

		if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
			add("platform", true, "darwin/arm64")
		} else {
			add("platform", false, fmt.Sprintf("%s/%s — VMs require darwin/arm64", runtime.GOOS, runtime.GOARCH))
		}

		// The Virtualization.framework concurrent-guest cap applies to macOS
		// guests only; linux pools are bounded by memory, not licensing.
		darwinCount := 0
		for _, p := range cfg.Pools {
			if p.OS == "darwin" {
				darwinCount += p.Count
			}
		}
		if darwinCount <= macOSGuestCap {
			add("macos-guest-cap", true, fmt.Sprintf("%d darwin slot(s) ≤ cap %d", darwinCount, macOSGuestCap))
		} else {
			add("macos-guest-cap", false, fmt.Sprintf(
				"darwin pools total %d slots, exceeding Virtualization.framework's %d-macOS-guest cap; the extra slots could never boot",
				darwinCount, macOSGuestCap,
			))
		}

		for target, gh := range clients {
			name := "runner-perm:" + target.String()
			pctx, cancel := bounded.WithTimeout(ctx, checkBudget)
			err := gh.CheckRunnerPerm(pctx)
			cancel()
			if err != nil {
				add(name, false, err.Error())
			} else {
				add(name, true, "installation token carries the runner-administration permission")
			}
		}

		for _, p := range cfg.Pools {
			name := "image-resolve:" + p.Name
			ref, err := oci.ParseRef(p.Image)
			if err != nil {
				add(name, false, err.Error())
				continue
			}
			rctx, cancel := bounded.WithTimeout(ctx, checkBudget)
			digest, err := oci.NewClient().Resolve(rctx, ref)
			cancel()
			if err != nil {
				add(name, false, err.Error())
			} else {
				add(name, true, fmt.Sprintf("%s → %s", ref, short(digest)))
			}
		}

		free, err := freeDiskGB(dir.String())
		switch {
		case err != nil:
			add("disk-headroom", false, err.Error())
		case free < 30:
			// Judged by df, never du — CoW clones lie to du (image economics).
			add("disk-headroom", false, fmt.Sprintf("%dGB free; <30GB risks mid-job disk exhaustion", free))
		default:
			add("disk-headroom", true, fmt.Sprintf("%dGB free", free))
		}

		return checks
	}
}

// sweepBudget bounds the whole startup registration sweep. Best-effort by
// design: anything the budget cuts off is retried on the next cold start.
const sweepBudget = time.Minute

func sweepRegistrations(ctx context.Context, log *slog.Logger, gh *github.Client, prefix string) {
	// Self-bounded: this runs under the un-deadlined signal context at
	// startup; a hung GitHub API must not stall daemon boot.
	sctx, cancel := bounded.WithTimeout(ctx, sweepBudget)
	defer cancel()
	runners, err := gh.ListRunners(sctx)
	if err != nil {
		log.Warn("sweep: listing runners failed; stale registrations may linger", "err", err)
		return
	}
	for _, r := range runners {
		if strings.HasPrefix(r.Name, prefix+"-") && r.Status == "offline" {
			if err := gh.DeleteRunner(sctx, r.ID); err != nil {
				log.Warn("sweep: deleting stale runner", "name", r.Name, "err", err)
			} else {
				log.Info("sweep: removed stale registration", "name", r.Name)
			}
		}
	}
}

func short(digest string) string {
	if len(digest) > 19 {
		return digest[:19]
	}
	return digest
}

// freeDiskGB: judged by statfs, never du — CoW clones lie to du.
func freeDiskGB(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize) / (1 << 30), nil
}
