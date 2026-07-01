// runnyd is the runner daemon: one crash-only state machine per runner slot,
// serving the runny.v1 control surface over a unix socket. Restart is a cold
// start by design: validate, sweep, run.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/clonefile"
	"github.com/bojanrajkovic/runny/internal/cycle"
	"github.com/bojanrajkovic/runny/internal/diskfree"
	"github.com/bojanrajkovic/runny/internal/github"
	"github.com/bojanrajkovic/runny/internal/guest"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/images"
	"github.com/bojanrajkovic/runny/internal/logring"
	"github.com/bojanrajkovic/runny/internal/oci"
	"github.com/bojanrajkovic/runny/internal/respawn"
	"github.com/bojanrajkovic/runny/internal/socket"
	"github.com/bojanrajkovic/runny/internal/sshx"
	"github.com/bojanrajkovic/runny/internal/statemachine"
	"github.com/bojanrajkovic/runny/internal/tart"
	"github.com/bojanrajkovic/runny/internal/versioncore"
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
	testConfig := flag.String("test-config", "", "validate a config file (local checks only), print a JSON verdict, and exit")
	flag.Parse()

	// Side-effect-free: no home, no config, no lock — so it works against an
	// uninstalled binary (the bundled-app exec probe) and a misconfigured host.
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	// The config-compat gate: the NEW binary validates the in-place config with
	// local checks only and prints a JSON verdict. Side-effect-free like -version
	// (no home, no lock, no network), so the bundled/on-disk new binary can run
	// it before an upgrade commits — answering "will the new binary accept this
	// config?", the question the old daemon's reload preflight structurally can't.
	if *testConfig != "" {
		return runTestConfig(*testConfig)
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
	// wedged guest's clone, and its divergence can tip
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
		// Reap stale runner tarballs from the shared download store, keeping
		// the newest few per flavor. Safe here and only here: cold start has no
		// live cycle, so nothing holds a clone of an about-to-be-deleted
		// version. Best-effort — disk hygiene must never refuse startup.
		if err := images.PruneRunnerCache(dir.RunnerCacheDir(), runnerCacheKeep); err != nil {
			logger.Warn("pruning runner cache failed; stale tarballs may linger", "err", err)
		}
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
	// across restarts — the sweep below depends on it.
	prefix, err := dir.InstancePrefix()
	if err != nil {
		return err
	}
	if err := home.ValidateRunnerNames(prefix, cfg.Pools); err != nil {
		return err
	}
	logger.Info("runner namespace", "prefix", prefix)

	// Registration sweep: cold start owns the world. Offline
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
			CloneFile: clonefile.Clone,
			RemoveAll: os.RemoveAll,
			GitHub:    gh,
			Dial: guest.Dialer{
				SSH: sshx.Config{
					User:     p.SSHUser,
					Password: p.SSHPassword,
					Timeout:  p.SSHTimeout.D(),
				},
				Hardening: p.SSHHardening,
			},
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

	// Drain coordination: the wedge escalation (a guest that survives
	// force-stop can only be reclaimed by process exit) and the config reload
	// share one drainer. It drives every slot to a
	// stable state (wedged, or paused in BACKOFF — running jobs finish
	// first), re-issuing commands on every status change so a dropped
	// command or the backoffWait timer-vs-pause race cannot stall the
	// drain, then exits for a launchd cold start.
	//
	// Two-step init: the exitGate closure references d.deferConfigParse, so d
	// must be declared before its struct literal is evaluated (closures capture
	// variables, not values — the variable must be in scope when the literal runs).
	var d *drainer
	d = &drainer{
		log:  logger,
		stop: stop,
		// The local exit gate, shared by both causes: before handing the
		// process to launchd, prove the on-disk config still parses — the
		// respawn loads it whether the drain was for a wedge or a reload,
		// and holding a drained-but-serving daemon beats a crash-looping
		// socketless one. Local file I/O only: no network work at the exit
		// seam (a refusal there would have no good answer).
		exitGate: func(acceptedSHA string) (bool, string) {
			return exitConfigVerdict(ctx, logger, configPath, prefix, acceptedSHA,
				d.deferConfigParse.Load(), deferralPlistPath(dir), execConfigTest)
		},
	}
	for _, s := range slots {
		d.slots = append(d.slots, s)
		s.OnChange(d.observe)
	}

	// Reload entry point, shared by the Reload RPC and SIGHUP.
	// Serialized so concurrent callers never run overlapping preflights
	// (each serialized call still revalidates fresh). It never gates on an
	// active drain: the imminent respawn loads the on-disk file whether or
	// not it was validated, so the verdict matters most then. The drain is
	// daemon-owned, never tied to the caller's context — a runnyctl that
	// disconnects after acceptance cannot orphan it.
	var reloadMu sync.Mutex
	// requestReload is the shared reload entry point for Reload RPC, UpgradeReload
	// RPC, and SIGHUP. deferToRespawnTarget is the UpgradeReload-only capability:
	// when true and the running binary's own parser rejects the config, the exit
	// gate may consult the respawn target's -test-config instead (forward-only
	// config edits that only the new binary can parse). Plain Reload and SIGHUP
	// always pass false — the verb is the access boundary; a bool could be forged.
	requestReload := func(ctx context.Context, source, reason string, deferToRespawnTarget bool) socket.ReloadResult {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		logger.Info("config reload requested", "source", source, "reason", reason)
		sha, failed, warnings := preflightReload(ctx, dir, configPath)
		// RPC-gated deferral: on a lone config-parse failure, UpgradeReload may
		// ask the respawn target whether it accepts the config. If so, clear the
		// failure and let the drain proceed; if not (stale symlink = target is the
		// old binary), substitute a synthetic check so the operator knows why.
		failed = parseDeferralCheck(ctx, configPath, deferralPlistPath(dir), failed, deferToRespawnTarget, execConfigTest)
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
		// Cause-gate BEFORE Start: d.Start synchronously rechecks convergence and
		// can spawn tryExit immediately (an already-stable fleet — operator-paused
		// slots), whose exit gate reads deferConfigParse. Setting it first closes
		// that race so an idle-fleet UpgradeReload never refuses on its own parse
		// before the flag lands. The flag only gates the exit gate, which never
		// runs until draining, so setting it pre-Start is safe; it also covers the
		// merge case (a wedge drain a later UpgradeReload joins mid-flight).
		//
		// System-daemon-only, same as the preflight deferral: a non-system daemon
		// (deferralPlistPath == "") respawns from a bundle-relative BundleProgram,
		// not the system plist, so it can't defer. Gating the flag here keeps the
		// exit gate consistent with the preflight — both refuse a per-user agent's
		// unparseable config honestly instead of pointing at a respawn target it
		// doesn't have.
		if deferToRespawnTarget && deferralPlistPath(dir) != "" {
			d.SetDeferConfigParse()
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
		return requestReload(ctx, "rpc", reason, false)
	}
	srv.UpgradeReloadFn = func(ctx context.Context, reason string) socket.ReloadResult {
		return requestReload(ctx, "rpc/upgrade", reason, true)
	}
	srv.DrainFn = d.State
	srv.PruneFn = func(ctx context.Context, apply bool) socket.PrunePlan {
		// Build the keep-set from live slot statuses.
		keepPaths := map[string]bool{}
		protectTarballs := map[string]bool{}
		for _, slot := range slots {
			st := slot.Status()
			if st.ImageDigest != "" {
				keepPaths[dir.ImageBundleDir(st.Image, st.ImageDigest)] = true
			}
			if st.RunnerVersion != "" {
				protectTarballs[st.RunnerVersion] = true
			}
		}
		// Protect tarballs whose download is in flight but whose RunnerVersion
		// has not been published to slot status yet (EnsureRunnerTarball
		// completes before Ensure returns, so there is a window where the
		// tarball is cached but the slot still shows RunnerVersion="").
		images.ProtectActiveTarballs(protectTarballs)
		// Resolve current digest for each configured pool ref.
		protectRefDirNames := map[string]bool{}
		configuredRefs := map[string]string{}
		var skips []socket.PruneSkip
		ociClient := oci.NewClient()
		for _, pool := range cfg.Pools {
			ref, err := oci.ParseRef(pool.Image)
			if err != nil {
				// Image ref unparseable — protect the ref dir rather than
				// letting PlanImageBundlePrune classify it as "removed pool".
				// Use pool.Image directly: if parsing fails we can't canonicalize.
				protectRefDirNames[filepath.Base(dir.ImageRefDir(pool.Image))] = true
				skips = append(skips, socket.PruneSkip{Ref: pool.Image, Reason: "image ref parse failed: " + err.Error()})
				continue
			}
			// Use ref.String() (canonical, tag-free for digest-pinned refs) so
			// refDirName matches the on-disk path, not pool.Image which may carry
			// a tag that sanitizeRef fails to strip when a digest is also present.
			refDirName := filepath.Base(dir.ImageRefDir(ref.String()))
			displayRef := pool.Image
			if i := strings.IndexByte(displayRef, '@'); i >= 0 {
				displayRef = displayRef[:i]
			}
			configuredRefs[refDirName] = displayRef
			rctx, cancel := bounded.WithTimeout(ctx, checkBudget)
			digest, resolveErr := ociClient.Resolve(rctx, ref)
			cancel()
			if resolveErr != nil {
				protectRefDirNames[refDirName] = true
				skips = append(skips, socket.PruneSkip{Ref: pool.Image, Reason: resolveErr.Error()})
				continue
			}
			keepPaths[dir.ImageBundleDir(ref.String(), digest)] = true
		}
		tarballItems, tarballErr := images.PlanRunnerCachePrune(dir.RunnerCacheDir(), runnerCacheKeep, protectTarballs)
		bundleItems, bundleErr := images.PlanImageBundlePrune(dir.ImagesDir(), keepPaths, protectRefDirNames, configuredRefs)
		combined := append(tarballItems, bundleItems...)
		allItems := make([]socket.PruneItem, len(combined))
		for i, it := range combined {
			allItems[i] = socket.PruneItem{Path: it.Path, Bytes: it.Bytes, Kind: it.Kind, Reason: it.Reason, Label: it.Label}
		}
		plan := socket.PrunePlan{Items: allItems, Applied: apply, Skips: skips}
		if tarballErr != nil {
			plan.Errors = append(plan.Errors, "runner-cache scan: "+tarballErr.Error())
		}
		if bundleErr != nil {
			plan.Errors = append(plan.Errors, "image-bundle scan: "+bundleErr.Error())
		}
		if apply {
			// Re-snapshot live slot state immediately before deleting to
			// protect artifacts adopted by slots that left BACKOFF during the
			// potentially-slow plan phase (registry resolves + dir walks).
			liveKeep := map[string]bool{}
			liveTarball := map[string]bool{}
			for _, slot := range slots {
				st := slot.Status()
				if st.ImageDigest != "" {
					liveKeep[dir.ImageBundleDir(st.Image, st.ImageDigest)] = true
				}
				if st.RunnerVersion != "" {
					liveTarball[st.RunnerVersion] = true
				}
			}
			images.ProtectActiveTarballs(liveTarball)
			plan.ReclaimedBytes, plan.ApplyErr = images.ApplyPrune(combined, func(it images.PlanItem) bool {
				switch it.Kind {
				case "image-bundle":
					return !liveKeep[it.Path]
				case "runner-tarball":
					return !liveTarball[strings.TrimSuffix(filepath.Base(it.Path), ".partial")]
				}
				return true
			})
		}
		return plan
	}
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

	// A system daemon also logs when it falls behind the binary launchd would
	// respawn it as, so a headless operator reading logs — not running runnyctl —
	// sees the same upgrade hint the CLI prints. The target is read from the
	// daemon's own LaunchDaemon plist, re-resolving symlinks now (a brew upgrade
	// repoints the opt-symlink without rewriting the plist); never from
	// os.Executable, which would always report the running version and so could
	// never detect a newer on-disk binary. Log-only — the daemon never respawns
	// itself. Only the system daemon has a plist to read; a per-user agent stays
	// quiet.
	if dir.String() == home.SystemHomeDir {
		notice := &upgradeNotice{
			log:     logger,
			running: versioncore.Core(version),
			resolve: func(ctx context.Context) string {
				v, ok := respawn.TargetVersion(ctx, dir)
				if !ok {
					return ""
				}
				return versioncore.Core(v)
			},
		}
		go notice.run(ctx)
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
			_ = requestReload(ctx, "SIGHUP", "", false)
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
// pools targeting different orgs or repos usually carry different Apps,
// so credentials are per-pool. Used by startup and by the
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
// ctx; no SSH or guest calls exist on this path.
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
//   - competing-registration is orthogonal to whether the new config is valid:
//     a leftover per-user agent must not refuse a reload, so it becomes a
//     warning (like local-network).
//   - private-key-perms is an App-key hygiene warning, not a validity failure:
//     a loose-perms key still authenticates, so it must not refuse a reload —
//     a warning, like the two above (see failedChecks for the full reasoning).
func splitPreflightChecks(checks []socket.DoctorCheck) (failed, warnings []socket.DoctorCheck) {
	for _, c := range checks {
		if c.OK {
			continue
		}
		switch c.Name {
		case "local-network", "competing-registration", privateKeyPermsCheck:
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

// failedChecks filters to the failures that should block daemon startup. Four
// checks are excluded as informational-at-startup:
//   - competing-registration: a leftover per-user agent (or an inconclusive
//     launchctl probe) is a latent conflict, not a reason to refuse boot — the
//     single-instance flock handles the actual runtime contention, and refusing
//     to start would be strictly worse (no daemon at all). It stays loud via
//     `runnyctl doctor`.
//   - config-drift: the file on disk differs from the running config; the
//     respawn re-reads the file AFTER the vms-sweep window, so a concurrent
//     re-template (Ansible) must not crash-loop startup on a check that does not
//     affect whether THIS config runs.
//   - local-network: at cold start no vmnet interface is up, so it is green
//     anyway; its one red-at-startup case — a self-daemonized runnyd macOS will
//     deny — is surfaced loudly (an Error log + a red `runnyctl doctor`/status)
//     but must not refuse boot, since foreground and launchd starts are fine and
//     even a denied daemon should run and report the cause rather than crash-loop.
//   - private-key-perms: a group/world-readable App key is a security-hygiene
//     warning, not an operational blocker — the daemon still authenticates with
//     it. The exit gate (localConfigChecks) does not check perms, so gating here
//     would let a mid-drain edit be green-lit by the exit gate then refused by
//     the respawn child (no daemon). Advisory, like competing-registration.
//
// All stay visible via `runnyctl doctor`, just not as a startup gate.
func failedChecks(checks []socket.DoctorCheck) []socket.DoctorCheck {
	var out []socket.DoctorCheck
	for _, c := range checks {
		if !c.OK && c.Name != "config-drift" && c.Name != "local-network" &&
			c.Name != "competing-registration" && c.Name != privateKeyPermsCheck {
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

// localConfigChecks runs the local, deterministic checks the respawn HARD-FAILS
// on at cold start and returns the failing ones: GitHub client construction
// (private-key read + PEM/RSA parse — local, no network), the macOS guest cap,
// the runner-name namespace, and per-pool image-ref parse. It is the single
// definition of "would the new binary survive its own local startup
// validation", consumed by the -test-config gate (testConfigVerdict) and the
// exit gate, so a check the respawn enforces can never be silently dropped from
// the gate again — the class of bug that once let a missing private key through.
// Network checks are deliberately excluded: upgrade-readiness is config-schema
// compatibility, not live GitHub/registry/disk health, and coupling them would
// let a transient blip refuse a valid upgrade. prefix is injected so the caller
// picks namespace resolution — the daemon's persisted prefix, or the gate's
// conservative worst-case.
func localConfigChecks(cfg *home.Config, prefix string) []socket.DoctorCheck {
	var failed []socket.DoctorCheck
	// github.New reads + parses the private key with no network; startup's
	// buildClients hard-fails on it, so the gate must mirror it (same reasoning as
	// the image-ref parse below) or it green-lights a respawn that crash-loops.
	if _, _, err := buildClients(cfg); err != nil {
		name := "github-client"
		var pe *poolClientError
		if errors.As(err, &pe) {
			name += ":" + pe.Pool
		}
		failed = append(failed, socket.DoctorCheck{Name: name, OK: false, Detail: err.Error()})
	}
	if c := checkMacOSGuestCap(cfg); !c.OK {
		failed = append(failed, c)
	}
	if err := home.ValidateRunnerNames(prefix, cfg.Pools); err != nil {
		failed = append(failed, socket.DoctorCheck{Name: "runner-namespace", OK: false, Detail: err.Error()})
	}
	for _, p := range cfg.Pools {
		if _, err := oci.ParseRef(p.Image); err != nil {
			failed = append(failed, socket.DoctorCheck{Name: "image-ref:" + p.Name, OK: false, Detail: err.Error()})
		}
	}
	return failed
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
		checks = append(checks, checkPrivateKeyPerms(cfg))

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
			// A per-user runnyd agent co-registered with this system daemon is the one
			// cross-shape conflict the ownership model no longer auto-resolves — surface
			// it loudly here so a headless operator can spot it in one command.
			checks = append(checks, checkCompetingRegistration(ctx, dir, configPath))
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

		var maxImageBytes int64
		for _, p := range cfg.Pools {
			name := "image-resolve:" + p.Name
			ref, err := oci.ParseRef(p.Image)
			if err != nil {
				add(name, false, err.Error())
				continue
			}
			rctx, cancel := bounded.WithTimeout(ctx, checkBudget)
			digest, diskBytes, err := oci.NewClient().ResolveWithDiskBytes(rctx, ref)
			cancel()
			if err != nil {
				add(name, false, err.Error())
			} else {
				cached := tart.Bundle(dir.ImageBundleDir(ref.String(), digest)).Verify() == nil
				cacheNote := ""
				if cached {
					cacheNote = " (cached)"
				}
				add(name, true, fmt.Sprintf("%s → %s (%s uncompressed%s)", ref, short(digest), oci.HumanBytes(diskBytes), cacheNote))
				if !cached && diskBytes > maxImageBytes {
					maxImageBytes = diskBytes
				}
			}
		}

		// Judged by statfs(2) / volumeAvailableCapacityForImportantUsageKey,
		// never du — CoW clones lie to du (image economics).
		free, err := diskfree.AvailableBytes(dir.String())
		if err != nil {
			add("disk-headroom", false, err.Error())
		} else {
			ok, detail := checkDiskHeadroom(free, maxImageBytes)
			add("disk-headroom", ok, detail)
		}

		return checks
	}
}

// sweepBudget bounds the whole startup registration sweep. Best-effort by
// design: anything the budget cuts off is retried on the next cold start.
const sweepBudget = time.Minute

// runnerCacheKeep is how many runner-tarball versions per flavor the cold-start
// prune retains in the shared download store. Two, not one: it covers a version
// rollover straddling a restart (the current build plus the one it just
// replaced) so neither is re-downloaded, while keeping the store tiny. Live
// cycles read their own clones, so this bounds only the download cache.
const runnerCacheKeep = 2

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

// checkDiskHeadroom returns the disk-headroom DoctorCheck. It is image-aware:
// the floor is max(configured image's uncompressed size + 2 GiB, 30 GiB) so
// the verdict matches the pull guard in internal/oci/oci.go, which refuses a
// pull when free space < uncompressed + 2 GiB. 30 GiB is the fallback when no
// image size is available (parse failure, no pools configured).
// freeBytes is the result of diskfree.AvailableBytes; maxImageBytes is the
// largest declared uncompressed image size across all successfully resolved
// pool images (0 when none resolved successfully).
func checkDiskHeadroom(freeBytes uint64, maxImageBytes int64) (bool, string) {
	const minFloor = 30 << 30 // 30 GiB — the pre-image-awareness floor
	floor := uint64(maxImageBytes) + oci.PullHeadroom
	if floor < minFloor {
		floor = minFloor
	}
	freeGB := freeBytes >> 30 // for display only
	if freeBytes < floor {
		floorGB := (floor + (1 << 30) - 1) >> 30 // ceiling for display
		if maxImageBytes > 0 {
			return false, fmt.Sprintf("%dGB free; need ≥%dGB to pull the largest configured image (%s uncompressed)", freeGB, floorGB, oci.HumanBytes(maxImageBytes))
		}
		return false, fmt.Sprintf("%dGB free; <%dGB risks mid-job disk exhaustion", freeGB, floorGB)
	}
	return true, fmt.Sprintf("%dGB free", freeGB)
}
