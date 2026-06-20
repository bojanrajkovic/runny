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
	"reflect"
	"runtime"
	"strings"
	"sync"
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
	configFlag := flag.String("config", "", "config path (default <home>/config.yaml)")
	checkOnly := flag.Bool("doctor", false, "run validation checks and exit")
	showVersion := flag.Bool("version", false, "print the daemon version and exit")
	flag.Parse()

	// Side-effect-free: no home, no config, no lock — so it works against an
	// uninstalled binary (the bundled-app exec probe) and a misconfigured host.
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	dir, err := home.ResolveServer()
	if err != nil {
		return err
	}
	_, systemHomeErr := os.Stat(home.SystemHomeDir)
	if err := systemHomeOwnershipError(dir, os.Geteuid(), systemHomeErr == nil); err != nil {
		return err
	}
	if err := dir.Ensure(); err != nil {
		return err
	}
	configPath := *configFlag
	if configPath == "" {
		configPath = dir.ConfigPath()
	}
	cfg, startupSHA, err := home.LoadConfigSHA(configPath)
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

	// Claim SIGHUP before the startup gauntlet, so its default
	// terminate-the-process disposition can never hard-kill runnyd mid-startup
	// — a scripted double-reload (`launchctl kill SIGHUP` twice), or a
	// foreground runnyd's terminal closing. The consuming goroutine starts
	// later, once the drainer exists; a signal arriving meanwhile waits in the
	// buffer and triggers a reload then. -doctor exits before the goroutine,
	// so the buffered signal is harmless there.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	// Logging: size-capped file sink + ring buffer, both structured.
	logFile, err := openRotatingFile(dir.LogFile(), logFileCap)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}
	defer logFile.Close()
	ring := logring.NewRing(4096)
	// Runner output gets its own ring: guest run.sh lines, the primary
	// `runnyctl logs` stream, kept apart from the daemon's structured log.
	runnerRing := logring.NewRing(4096)
	// Read the launch context once, through the launchContextNow seam the rest of
	// the daemon uses: it decides where the log goes (a foreground runnyd also
	// tees to stderr so the operator sees it live; a launchd-started or orphaned
	// one writes to the file + ring only — launchd captures its own
	// stdout/stderr, an orphan has no terminal) and whether to raise the loud
	// orphaned warning below.
	launchCtx := launchContextNow()
	logger := slog.New(logring.NewHandler(logSinkFor(launchCtx, logFile, os.Stderr), slog.LevelDebug, ring))
	slog.SetDefault(logger)
	// config_sha256 chains the audit trail across restarts: a reload logs
	// the hash it validated, and the respawn logs the hash it loaded — same
	// hash, same file, provably.
	logger.Info("runnyd starting", "version", version, "home", dir.String(),
		"config_sha256", startupSHA)

	// Sweep the vms dir BEFORE validation: teardown deliberately retains a
	// wedged guest's clone (ADR-0012), and its divergence can tip
	// disk-headroom under the threshold — validating first would crash-loop
	// every respawn on a leak only this sweep can free. The sweep depends on
	// nothing (not prefix, not clients, not network) and runs only on the
	// real-startup path, under the instance lock — never in -doctor mode,
	// which is read-only and runs alongside a live daemon whose clones must
	// not be deleted.
	if !*checkOnly {
		if err := os.RemoveAll(dir.VMsDir()); err != nil {
			return fmt.Errorf("sweeping vms dir: %w", err)
		}
		if err := dir.Ensure(); err != nil {
			return err
		}
		logger.Info("swept vms dir")
	}

	clients, distinctClients, err := buildClients(cfg)
	if err != nil {
		return err
	}

	doctor := makeDoctor(dir, configPath, cfg, distinctClients)
	if *checkOnly {
		return runDoctor(doctor) // read-only: runs fine alongside a live daemon
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Loud, early signal for the one launch context macOS silently denies: a
	// self-daemonized / reparented runnyd (launchd did not start it, no live
	// parent) cannot reach its guests. Not fatal — foreground and launchd starts
	// are fine, and this also surfaces via the local-network doctor/status check
	// — but logged at Error so a headless operator sees it before the first cycle
	// dies at AWAIT_SSH. launchOrphaned is unreachable off darwin (the launch
	// context is darwin-only in meaning), so no GOOS guard is needed.
	if launchCtx == launchOrphaned {
		logger.Error(orphanedDenyDetail)
	}

	// Startup validation: fail loudly now, not silently later.
	if failed := failedChecks(doctor(ctx)); len(failed) > 0 {
		for _, c := range failed {
			logger.Error("startup check failed", "check", c.Name, "detail", c.Detail)
		}
		return fmt.Errorf("%d startup check(s) failed (run runnyd -doctor for detail)", len(failed))
	}

	// This install's runner-name namespace: <slug(hostname)>-<rand8>, derived
	// and persisted (not configured) so it can't be mistyped and stays stable
	// across restarts — the sweep below depends on it (ADR-0009).
	prefix, err := dir.InstancePrefix()
	if err != nil {
		return err
	}
	if err := home.ValidateRunnerNames(prefix, cfg.Pools); err != nil {
		return err
	}
	logger.Info("runner namespace", "prefix", prefix)

	// Registration sweep: cold start owns the world (ADR-0004). Offline
	// registrations carrying our prefix, on every target. (The vms-dir sweep
	// already ran, before validation.)
	for _, c := range distinctClients {
		sweepRegistrations(ctx, logger, c, prefix)
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
		gh, osName := clients[ghKey{appID: p.GitHub.AppID, target: p.Target}], p.OS
		deps := statemachine.Deps{
			Home:           dir,
			Config:         cfg,
			Pool:           p,
			InstancePrefix: prefix,
			VM:             vmManager(),
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
			GitHub: gh,
			Dial: guest.Dialer{SSH: sshx.Config{
				User:     p.SSHUser,
				Password: p.SSHPassword,
				Timeout:  p.SSHTimeout.D(),
			}},
			Log: logger,
			OnRunnerLine: func(slot, cycleID, line string) {
				runnerRing.Add(logring.Entry{
					Time: time.Now(), Level: "runner", Message: line,
					Attrs: map[string]string{"slot": slot, "cycle": cycleID},
				})
			},
		}
		for i := 1; i <= p.Count; i++ {
			slots = append(slots, statemachine.NewSlot(fmt.Sprintf("%s-%d", p.Name, i), deps))
		}
	}

	srv := socket.NewServer(slots, ring, runnerRing,
		func(slot string) cycle.Store { return cycle.Store{SlotDir: dir.SlotCyclesDir(slot)} },
		doctor, version, cfg)
	// Publish the hash of the file THIS process loaded, so a reload follower
	// can prove the respawn came up on the config its preflight vetted.
	srv.ConfigSHA256 = startupSHA

	// Drain coordination: the wedge escalation (ADR-0012 — a guest that
	// survives force-stop can only be reclaimed by process exit) and the
	// config reload (ADR-0014) share one drainer. It drives every slot to a
	// stable state (wedged, or paused in BACKOFF — running jobs finish
	// first), re-issuing commands on every status change so a dropped
	// command or the backoffWait timer-vs-pause race cannot stall the
	// drain, then exits for a launchd cold start.
	d := &drainer{
		log:  logger,
		stop: stop,
		// The local exit gate, shared by both causes: before handing the
		// process to launchd, prove the on-disk config still parses — the
		// respawn loads it whether the drain was for a wedge or a reload,
		// and holding a drained-but-serving daemon beats a crash-looping
		// socketless one. Local file I/O only: no network work at the exit
		// seam (a refusal there would have no good answer).
		exitGate: func(acceptedSHA string) (bool, string) {
			// One read: the parse check and the hash describe the same bytes,
			// so a concurrent atomic replace can't make us hold on version A's
			// parse while warning about version B's hash (or vice versa).
			cfg, sha, err := home.LoadConfigSHA(configPath)
			if err != nil {
				return false, fmt.Sprintf("config.yaml no longer parses; the respawn would refuse it: %v", err)
			}
			// Re-run the local startup checks the respawn hard-fails on: a
			// mid-drain edit that parses but overflows the darwin guest cap or
			// the runner-name length would otherwise crash-loop a socketless
			// respawn. Network checks stay off the exit seam — a refusal there
			// has no good answer (hold a drained fleet on a GitHub blip?).
			for _, c := range []socket.DoctorCheck{checkMacOSGuestCap(cfg), checkRunnerNamespace(dir, cfg)} {
				if !c.OK {
					return false, fmt.Sprintf("the respawn would refuse %s: %s", c.Name, c.Detail)
				}
			}
			if acceptedSHA != "" && sha != acceptedSHA {
				logger.Warn("config changed during the drain; the respawn will validate and load the newer file",
					"accepted_sha256", acceptedSHA, "current_sha256", sha)
			}
			return true, ""
		},
	}
	for _, s := range slots {
		d.slots = append(d.slots, s)
		s.OnChange(d.observe)
	}

	// Reload entry point (ADR-0014), shared by the Reload RPC and SIGHUP.
	// Serialized so concurrent callers never run overlapping preflights
	// (each serialized call still revalidates fresh). It never gates on an
	// active drain: the imminent respawn loads the on-disk file whether or
	// not it was validated, so the verdict matters most then. The drain is
	// daemon-owned, never tied to the caller's context — a runnyctl that
	// disconnects after acceptance cannot orphan it.
	var reloadMu sync.Mutex
	requestReload := func(ctx context.Context, source, reason string) socket.ReloadResult {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		logger.Info("config reload requested", "source", source, "reason", reason)
		sha, failed, warnings := preflightReload(ctx, dir, configPath)
		for _, c := range warnings {
			logger.Warn("reload validation warning (not blocking)", "check", c.Name, "detail", c.Detail)
		}
		if len(failed) > 0 {
			for _, c := range failed {
				logger.Error("reload validation failed", "check", c.Name, "detail", c.Detail)
			}
			logger.Error("config reload refused: the new config failed validation; the running daemon is unchanged",
				"failed", len(failed), "config_sha256", sha)
			return socket.ReloadResult{
				FailedChecks: failed,
				Warnings:     warnings,
				Draining:     d.Reason(),
				ConfigSHA256: sha,
			}
		}
		logger.Info("config reload accepted", "config_sha256", sha)
		// Operator-paused slots are meaningful only when no drain is active
		// yet: once a drain (wedge or earlier reload) is running it pauses
		// every slot as mechanism, so reporting them here would mislabel
		// drain-paused slots as operator holds and send a log-auditor hunting
		// for a phantom pause.
		var pausedSlots []string
		if d.Reason() == "" {
			for _, s := range slots {
				if st := s.Status(); st.Paused && !st.Wedged {
					pausedSlots = append(pausedSlots, s.Name())
				}
			}
			if len(pausedSlots) > 0 {
				logger.Warn("operator-paused slots will resume after the respawn (pause is in-memory)",
					"slots", strings.Join(pausedSlots, ", "))
			}
		}
		drainReason := "config reload (" + source + ")"
		if reason != "" {
			drainReason += ": " + reason
		}
		startedDrain := d.Start(drainReason, sha)
		if !startedDrain {
			// A drain was already active (wedge, or an earlier reload): the
			// first reason wins on the status surface, but the supplied
			// reason stays durable in the log — the audit trail never loses
			// it. Supersede the prior cause's accepted hash with this
			// freshly-validated file, so the exit gate's "changed during the
			// drain" comparison is against what was actually vetted (a wedge
			// records ""; an earlier reload records its own, now stale).
			d.UpdateAcceptedSHA(sha)
			logger.Info("reload requested while already draining; supplied reason recorded here",
				"reason", reason, "draining", d.Reason())
		}
		d.recheck() // unblocks a held exit gate when the operator just fixed the file
		return socket.ReloadResult{
			Accepted:            true,
			StartedDrain:        startedDrain,
			Warnings:            warnings,
			Draining:            d.Reason(),
			SlotCount:           len(slots),
			OperatorPausedSlots: pausedSlots,
			ConfigSHA256:        sha,
		}
	}
	srv.ReloadFn = func(ctx context.Context, reason string) socket.ReloadResult {
		return requestReload(ctx, "rpc", reason)
	}
	srv.DrainFn = d.State
	// Deliver drain-progress bumps (slot transitions, exit-gate hold flips) to
	// watchers immediately, so a follower's stall timer tracks real progress
	// rather than the 30s heartbeat.
	d.onProgress = srv.NotifyProgress

	// Publish the daemon's Local Network (TCC) grant so the app can surface the
	// grant affordance proactively — before the first guest dial fails as "no
	// route to host". Sampled off the status hot path (a probe dial is up to 2s)
	// and cached; darwin-only, since the grant and the vmnet subnet it gates are
	// macOS concepts — the doctor's local-network check gates on GOOS the same way.
	if runtime.GOOS == "darwin" {
		// Wire the read Fn AND the change hook before starting the goroutine, so the
		// first sample's notify pushes a snapshot that already reads the sampler.
		sampler := &localNetworkSampler{onChange: srv.NotifyProgress}
		srv.LocalNetworkGrantFn = sampler.read
		go sampler.run(ctx)
	}

	// SIGHUP maps to the same validated reload path; the channel was claimed
	// before the startup gauntlet (above) so the default process-terminate
	// disposition is already disarmed — a SIGHUP, or a foreground runnyd's
	// terminal closing, drains gracefully instead of hard-killing mid-job.
	// The channel is dedicated and never joins signal.NotifyContext, which
	// would cancel the root context.
	go func() {
		for range hup {
			if d.Reason() != "" {
				// Drain already running: skip the network preflight (storms
				// must not burn token mints + registry resolves per signal);
				// run the cheap local stage only, and scream if it fails —
				// the respawn will load this file.
				if _, err := home.LoadConfig(configPath); err != nil {
					logger.Error("SIGHUP during drain: on-disk config is invalid and the respawn will load it", "err", err)
				}
				d.recheck()
				continue
			}
			_ = requestReload(ctx, "SIGHUP", "")
		}
	}()

	var wg sync.WaitGroup
	for _, s := range slots {
		wg.Go(func() { s.Run(ctx) })
	}
	socketPath := dir.SocketPath()
	logger.Info("slots running", "count", len(slots), "socket", socketPath)

	err = srv.Serve(ctx, socketPath)
	wg.Wait()
	logger.Info("runnyd stopped")
	if d.Exited() && err == nil {
		// Non-zero exit so launchd (KeepAlive) restarts us — deliberately
		// not a success exit, which a future SuccessfulExit-style plist
		// tweak would leave down silently. The cold start sweeps the vms
		// dir (reclaiming a leaked guest) and loads the on-disk config.
		err = fmt.Errorf("restarting after drain: %s", d.Reason())
	}
	return err
}

// systemHomeOwnershipError fails a botched system-daemon install loudly and
// clearly. A non-root service account (uid below the 500 login-user floor) that
// resolved to a per-user home because it does NOT own an existing SystemHomeDir
// is a broken install — without this it would crash-loop on a cryptic
// `mkdir /var/empty/.runny: permission denied`. A login user (uid >= 500) running
// a per-user agent beside a system install legitimately falls back, so the check
// is scoped to the service-uid range and to an existing system home.
func systemHomeOwnershipError(dir home.Dir, euid int, systemHomeExists bool) error {
	if euid <= 0 || euid >= 500 || string(dir) == home.SystemHomeDir || !systemHomeExists {
		return nil
	}
	return fmt.Errorf("running as a system service account (uid %d) but %s is not owned by it; "+
		"reinstall the system daemon with `sudo runnyctl install-daemon`", euid, home.SystemHomeDir)
}

// poolClientError attributes a GitHub client construction failure to its
// pool, so the reload preflight can name the failing check.
type poolClientError struct {
	Pool string
	Err  error
}

func (e *poolClientError) Error() string { return fmt.Sprintf("pool %s: %v", e.Pool, e.Err) }
func (e *poolClientError) Unwrap() error { return e.Err }

// buildClients constructs one GitHub client per distinct (App, target):
// pools targeting different orgs or repos usually carry different Apps
// (ADR-0009), so credentials are per-pool. Used by startup and by the
// reload preflight — client construction runs BEFORE the doctor suite at
// startup, so the preflight must replay it too (a deleted private key
// would pass a parse-only preflight and crash-loop the respawn).
func buildClients(cfg *home.Config) (map[ghKey]*github.Client, []*github.Client, error) {
	clients := map[ghKey]*github.Client{}
	var distinct []*github.Client
	for _, p := range cfg.Pools {
		key := ghKey{appID: p.GitHub.AppID, target: p.Target}
		if _, ok := clients[key]; ok {
			continue
		}
		c, err := github.New(github.Config{
			AppID:          p.GitHub.AppID,
			PrivateKeyPath: p.GitHub.PrivateKeyPath,
			APIBase:        p.GitHub.APIBase,
		}, github.Target(p.Target))
		if err != nil {
			return nil, nil, &poolClientError{Pool: p.Name, Err: err}
		}
		clients[key] = c
		distinct = append(distinct, c)
	}
	return clients, distinct, nil
}

// preflightReload runs the full startup gauntlet against the on-disk
// config — config-parse (LoadConfig: strict parse + defaults + validate),
// github-client:<pool> (client construction), then the whole doctor suite
// against the candidate config and clients — so a reload can only be
// accepted when the respawn's own startup validation would pass on the
// same inputs. Each network check self-bounds (checkBudget) regardless of
// ctx; no SSH or guest calls exist on this path (ADR-0011).
func preflightReload(ctx context.Context, dir home.Dir, configPath string) (sha string, failed, warnings []socket.DoctorCheck) {
	// One read: the SHA describes exactly the bytes validated below, so the
	// accepted hash provably names the file version this preflight vetted.
	newCfg, sha, err := home.LoadConfigSHA(configPath)
	if err != nil {
		return sha, []socket.DoctorCheck{{Name: "config-parse", OK: false, Detail: err.Error()}}, nil
	}
	_, newClients, err := buildClients(newCfg)
	if err != nil {
		name := "github-client"
		var pe *poolClientError
		if errors.As(err, &pe) {
			name += ":" + pe.Pool
		}
		return sha, []socket.DoctorCheck{{Name: name, OK: false, Detail: err.Error()}}, nil
	}
	failed, warnings = splitPreflightChecks(makeDoctor(dir, configPath, newCfg, newClients)(ctx))
	return sha, failed, warnings
}

// splitPreflightChecks post-filters the gauntlet's results into the reload
// verdict, adjusting for the preflight running in a different environment
// (guests up) than the respawn (cold):
//
//   - local-network only asserts when a vmnet interface is up — true at
//     reload time, false at the respawn's cold start, where it reports
//     informational green. A flake here must not refuse a reload over a
//     check the respawn cannot fail and no config edit can affect; it
//     becomes a warning.
//   - disk-headroom stays a refusal (fail-safe), but at preflight it counts
//     live clones' divergence that the drain + respawn sweep will free, so
//     the detail says why the number may differ.
func splitPreflightChecks(checks []socket.DoctorCheck) (failed, warnings []socket.DoctorCheck) {
	for _, c := range checks {
		if c.OK {
			continue
		}
		switch c.Name {
		case "local-network":
			warnings = append(warnings, c)
		case "disk-headroom":
			c.Detail += " (measured with guests running; the respawn sweeps clones before re-checking — free space or retry)"
			failed = append(failed, c)
		default:
			failed = append(failed, c)
		}
	}
	return failed, warnings
}

// failedChecks filters to the failures that should block daemon startup. Two
// checks are excluded as informational-at-startup:
//   - config-drift: the file on disk differs from the running config; the
//     respawn re-reads the file AFTER the vms-sweep window, so a concurrent
//     re-template (Ansible) must not crash-loop startup on a check that does not
//     affect whether THIS config runs.
//   - local-network: at cold start no vmnet interface is up, so it is green
//     anyway; its one red-at-startup case — a self-daemonized runnyd macOS will
//     deny — is surfaced loudly (an Error log + a red `runnyctl doctor`/status)
//     but must not refuse boot, since foreground and launchd starts are fine and
//     even a denied daemon should run and report the cause rather than crash-loop.
//
// Both stay visible via `runnyctl doctor`, just not as a startup gate.
func failedChecks(checks []socket.DoctorCheck) []socket.DoctorCheck {
	var out []socket.DoctorCheck
	for _, c := range checks {
		if !c.OK && c.Name != "config-drift" && c.Name != "local-network" {
			out = append(out, c)
		}
	}
	return out
}

// checkMacOSGuestCap and checkRunnerNamespace are the pure-local, deterministic
// startup checks the exit gate re-runs against a mid-drain-edited config (the
// respawn hard-fails on them, so a parse-only gate would let an edit that
// overflows the guest cap or the runner-name length crash-loop the socketless
// respawn). Shared with makeDoctor so the gate and startup agree by construction.
func checkMacOSGuestCap(cfg *home.Config) socket.DoctorCheck {
	darwinCount := 0
	for _, p := range cfg.Pools {
		if p.OS == "darwin" {
			darwinCount += p.Count
		}
	}
	if darwinCount <= macOSGuestCap {
		return socket.DoctorCheck{
			Name: "macos-guest-cap", OK: true,
			Detail: fmt.Sprintf("%d darwin slot(s) ≤ cap %d", darwinCount, macOSGuestCap),
		}
	}
	return socket.DoctorCheck{Name: "macos-guest-cap", OK: false, Detail: fmt.Sprintf(
		"darwin pools total %d slots, exceeding Virtualization.framework's %d-macOS-guest cap; the extra slots could never boot",
		darwinCount, macOSGuestCap,
	)}
}

func checkRunnerNamespace(dir home.Dir, cfg *home.Config) socket.DoctorCheck {
	prefix, err := dir.InstancePrefix()
	if err != nil {
		return socket.DoctorCheck{Name: "runner-namespace", OK: false, Detail: err.Error()}
	}
	if err := home.ValidateRunnerNames(prefix, cfg.Pools); err != nil {
		return socket.DoctorCheck{Name: "runner-namespace", OK: false, Detail: err.Error()}
	}
	return socket.DoctorCheck{Name: "runner-namespace", OK: true, Detail: prefix}
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
// ghKey dedups github clients: one per distinct (App, registration target).
type ghKey struct {
	appID  int64
	target home.TargetConfig
}

func makeDoctor(dir home.Dir, configPath string, cfg *home.Config, clients []*github.Client) func(context.Context) []socket.DoctorCheck {
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

		// Config drift: behavior comes from the config loaded at startup
		// (cfg), not the file on disk. Post-defaulting struct equality —
		// comment/reorder edits don't false-positive. Trivially green at
		// startup (the file was just loaded), so it can never block daemon
		// entry; under reload validation cfg IS the on-disk candidate, so
		// drift can never block the reload it recommends.
		if onDisk, err := home.LoadConfig(configPath); err != nil {
			add("config-drift", false, err.Error())
		} else if !reflect.DeepEqual(onDisk, cfg) {
			add("config-drift", false, "config.yaml differs from the running config; apply with `runnyctl reload`")
		} else {
			add("config-drift", true, "config.yaml matches the running config")
		}

		// Runner namespace: derives (and persists, first run) the instance
		// prefix — which also proves instance-id is writable, a startup
		// requirement a doctor pass must not paper over — and validates the
		// assembled runner names against GitHub's length cap, which would
		// otherwise surface as a permanent non-retryable 422 loop at MINT_JIT.
		// The Virtualization.framework concurrent-guest cap applies to macOS
		// guests only; linux pools are bounded by memory, not licensing. Both
		// are shared with the exit gate (pure-local, deterministic).
		checks = append(checks, checkRunnerNamespace(dir, cfg), checkMacOSGuestCap(cfg))

		// Local Network (TCC): a self-daemonized / reparented runnyd (one launchd
		// did not start) is silently denied vmnet access, so guest dials fail "no
		// route to host"; a launchd-started daemon of any uid is auto-allowed. The
		// orphaned context fails this check immediately; otherwise the probe only
		// asserts once a vmnet interface is up, so at idle startup it is
		// informational — run `runnyd -doctor` (or the Doctor RPC) while a guest is
		// running to confirm the grant. Only meaningful on darwin (docs/deploy.md).
		if runtime.GOOS == "darwin" {
			ok, detail := localNetworkReach()
			add("local-network", ok, detail)
		}

		for _, gh := range clients {
			name := "runner-perm:" + gh.Target().String()
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
