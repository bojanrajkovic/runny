//go:build windows

package sysdaemon

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/bojanrajkovic/runny/internal/home"
)

// fakeService is a scmService double. All calls append to a log shared with
// fakeMgr and orderedRun, so tests can assert cross-seam ordering (service
// before icacls) without threading two separate call histories together.
type fakeService struct {
	name        string
	status      svc.Status
	calls       *[]string
	lastConfig  mgr.Config
	neverStops  bool // simulates a service the SCM never actually stops
	settleAfter int  // Control rejects with ERROR_SERVICE_CANNOT_ACCEPT_CTRL this many times before stopping
}

func (f *fakeService) UpdateConfig(c mgr.Config) error {
	*f.calls = append(*f.calls, "update:"+f.name)
	f.lastConfig = c
	return nil
}

func (f *fakeService) SetRecoveryActions(actions []mgr.RecoveryAction, resetPeriod uint32) error {
	*f.calls = append(*f.calls, "recovery")
	return nil
}

func (f *fakeService) SetRecoveryActionsOnNonCrashFailures(flag bool) error {
	*f.calls = append(*f.calls, fmt.Sprintf("nonCrashRecovery:%v", flag))
	return nil
}

func (f *fakeService) Start(args ...string) error {
	*f.calls = append(*f.calls, "start")
	return nil
}

func (f *fakeService) Control(c svc.Cmd) (svc.Status, error) {
	*f.calls = append(*f.calls, "stop")
	if f.neverStops {
		return f.status, nil
	}
	if f.settleAfter > 0 {
		f.settleAfter--
		return f.status, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL
	}
	if f.status.State == svc.Stopped {
		return f.status, windows.ERROR_SERVICE_NOT_ACTIVE
	}
	f.status.State = svc.Stopped
	return f.status, nil
}

func (f *fakeService) Query() (svc.Status, error) { return f.status, nil }

func (f *fakeService) Delete() error {
	*f.calls = append(*f.calls, "delete")
	return nil
}

func (f *fakeService) Close() error { return nil }

// fakeMgr is a scmMgr double: existing non-nil makes OpenService succeed (the
// idempotent-reinstall / uninstall-existing path); nil makes it report
// ERROR_SERVICE_DOES_NOT_EXIST (the fresh-install / already-uninstalled path).
type fakeMgr struct {
	existing scmService
	calls    *[]string
}

func (f *fakeMgr) OpenService(name string) (scmService, error) {
	if f.existing == nil {
		return nil, windows.ERROR_SERVICE_DOES_NOT_EXIST
	}
	return f.existing, nil
}

func (f *fakeMgr) CreateService(name, exepath string, c mgr.Config) (scmService, error) {
	*f.calls = append(*f.calls, "create:"+name)
	// Stopped, not Running: the real SCM's CreateService does not start the
	// service — Install's own Start() call does that.
	s := &fakeService{name: name, calls: f.calls, lastConfig: c, status: svc.Status{State: svc.Stopped}}
	f.existing = s
	return s, nil
}

func (f *fakeMgr) Disconnect() error { return nil }

// orderedRun is a fake Runner that records raw icacls calls (for exactCall
// assertions) AND appends into the same shared order log the SCM fakes use,
// so a test can assert ordering across both seams.
type orderedRun struct {
	calls [][]string
	order *[]string
}

func (r *orderedRun) run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	*r.order = append(*r.order, "run:"+name)
	return "", nil
}

func newTestScmInstaller(m *fakeMgr, r *orderedRun) *scmInstaller {
	return &scmInstaller{
		cfg:       Config{Operator: testOperator, RunnydPath: `C:\runny\runnyd.exe`},
		connect:   func() (scmMgr, error) { return m, nil },
		run:       r.run,
		writeFile: func(string, []byte, os.FileMode) error { return nil },
		mkdirAll:  func(string, os.FileMode) error { return nil },
		removeAll: func(string) error { return nil },
		log:       func(string, ...any) {},
	}
}

func newStagingScmInstaller(m *fakeMgr, r *orderedRun, testConfig verdictTester) *scmInstaller {
	inst := newTestScmInstaller(m, r)
	inst.testConfig = testConfig
	return inst.WithStage(StagePlan{Config: []byte("pools: []\n")})
}

// Regression test: a freshly created service used to be registered
// StartAutomatic immediately, before its staged config was validated — a
// reboot before the operator fixed a bad config would start the daemon
// against it. A fresh service must stay demand-start until staging succeeds.
func TestScmInstallEnablesAutoStartAfterValidStaging(t *testing.T) {
	var order []string
	m := &fakeMgr{calls: &order}
	r := &orderedRun{order: &order}
	inst := newStagingScmInstaller(m, r, func(context.Context, string, string) (home.Verdict, error) {
		return home.Verdict{Status: home.VerdictOK}, nil
	})
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	created, ok := m.existing.(*fakeService)
	if !ok {
		t.Fatal("service was never created")
	}
	if created.lastConfig.StartType != mgr.StartAutomatic {
		t.Errorf("StartType = %v, want StartAutomatic once staging validates", created.lastConfig.StartType)
	}
}

func TestScmInstallKeepsFreshServiceDemandStartOnFailedStaging(t *testing.T) {
	var order []string
	m := &fakeMgr{calls: &order}
	r := &orderedRun{order: &order}
	inst := newStagingScmInstaller(m, r, func(context.Context, string, string) (home.Verdict, error) {
		return home.Verdict{Status: home.VerdictError, Errors: []string{"bad config"}}, nil
	})
	if err := inst.Install(context.Background()); err == nil {
		t.Fatal("Install must fail on a blocking verdict")
	}
	created, ok := m.existing.(*fakeService)
	if !ok {
		t.Fatal("service was never created")
	}
	if created.lastConfig.StartType != mgr.StartManual {
		t.Errorf("StartType = %v, want StartManual — a service whose staged config failed validation "+
			"must not be registered to auto-start on the next reboot", created.lastConfig.StartType)
	}
	if slices.Index(order, "start") >= 0 {
		t.Error("must not start a service whose staged config failed validation")
	}
}

// Regression guard for the other direction: reinstalling an EXISTING service
// must stay automatic even when the NEW staged config fails validation — it
// was already validated by a prior successful install, and downgrading it
// here would silently disable auto-start for an install still running fine.
// Regression test for the retry case Codex found in the previous fix:
// ensureService's update branch used to unconditionally set StartAutomatic
// before staging validated, so a reinstall (or a retry over a service a
// prior failed attempt left demand-start) whose NEW config also fails
// validation would leave the service registered to auto-start against a
// config that never actually passed runnyd -test-config. The service must
// stay (or become) demand-start through any failed staging attempt,
// existing or freshly created — ensureService always sets StartManual now;
// only a successful stage() reaching the end of Install re-enables it.
func TestScmInstallKeepsExistingServiceDemandStartOnFailedStaging(t *testing.T) {
	var order []string
	existing := &fakeService{
		name: WindowsServiceName, calls: &order,
		status:     svc.Status{State: svc.Running},
		lastConfig: mgr.Config{StartType: mgr.StartAutomatic}, // left behind by a prior successful install
	}
	m := &fakeMgr{existing: existing, calls: &order}
	r := &orderedRun{order: &order}
	inst := newStagingScmInstaller(m, r, func(context.Context, string, string) (home.Verdict, error) {
		return home.Verdict{Status: home.VerdictError, Errors: []string{"bad config"}}, nil
	})
	if err := inst.Install(context.Background()); err == nil {
		t.Fatal("Install must fail on a blocking verdict")
	}
	if existing.lastConfig.StartType != mgr.StartManual {
		t.Errorf("StartType = %v, want StartManual — a service must not stay (or become) auto-start when its "+
			"newly staged config failed validation, even if it was already Automatic before this run",
			existing.lastConfig.StartType)
	}
	if slices.Index(order, "start") >= 0 {
		t.Error("must not start a service whose staged config failed validation")
	}
}

// Closes the loop on the above: a retry whose config now validates must
// re-enable auto-start on a service a prior failed attempt left demand-start.
func TestScmInstallEnablesAutoStartOnRetryAfterPriorFailure(t *testing.T) {
	var order []string
	existing := &fakeService{
		name: WindowsServiceName, calls: &order,
		status:     svc.Status{State: svc.Stopped},
		lastConfig: mgr.Config{StartType: mgr.StartManual}, // left behind by a prior failed attempt
	}
	m := &fakeMgr{existing: existing, calls: &order}
	r := &orderedRun{order: &order}
	inst := newStagingScmInstaller(m, r, func(context.Context, string, string) (home.Verdict, error) {
		return home.Verdict{Status: home.VerdictOK}, nil
	})
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if existing.lastConfig.StartType != mgr.StartAutomatic {
		t.Errorf("StartType = %v, want StartAutomatic — a retry whose config now validates must enable auto-start",
			existing.lastConfig.StartType)
	}
}

// Regression test: staging used to run AFTER the ACL lockdown, so
// --operator naming a different account than whoever is running this
// (elevated) process would hit access denied — the locked-down ACL only
// grants Modify to the service SID and --operator. Staging must happen
// while the home still carries ProgramData's default ACL.
// The home's ACL is locked down BEFORE anything is written into it — a
// staged config or key must never sit under ProgramData's default ACL, even
// momentarily. --operator naming an account other than whoever is running
// this elevated process is a known, unsupported combination (a plain,
// immediate AccessDenied from the write below, not a silent gap) — see
// ensureHome's doc comment.
func TestScmInstallLocksDownACLBeforeStaging(t *testing.T) {
	var order []string
	m := &fakeMgr{calls: &order}
	r := &orderedRun{order: &order}
	inst := newStagingScmInstaller(m, r, func(context.Context, string, string) (home.Verdict, error) {
		return home.Verdict{Status: home.VerdictOK}, nil
	})
	inst.writeFile = func(path string, data []byte, perm os.FileMode) error {
		order = append(order, "write:"+path)
		return nil
	}
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	writeIdx := slices.IndexFunc(order, func(s string) bool { return strings.HasPrefix(s, "write:") })
	icaclsIdx := slices.Index(order, "run:icacls")
	if writeIdx < 0 {
		t.Fatal("config was never staged")
	}
	if icaclsIdx < 0 {
		t.Fatal("no icacls call was made")
	}
	if icaclsIdx > writeIdx {
		t.Errorf("the ACL lockdown (idx %d) must happen BEFORE staging (idx %d)", icaclsIdx, writeIdx)
	}
}

func TestScmInstallOrdersServiceBeforeHomeACL(t *testing.T) {
	var order []string
	m := &fakeMgr{calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	createIdx := slices.Index(order, "create:"+WindowsServiceName)
	firstIcaclsIdx := slices.Index(order, "run:icacls")
	if createIdx < 0 {
		t.Fatal("service was never created")
	}
	if firstIcaclsIdx < 0 {
		t.Fatal("no icacls call was made")
	}
	if createIdx > firstIcaclsIdx {
		t.Errorf("service must be created (idx %d) BEFORE any icacls call (idx %d) — the SID does not resolve until then",
			createIdx, firstIcaclsIdx)
	}
}

func TestScmInstallConfiguresRecovery(t *testing.T) {
	var order []string
	m := &fakeMgr{calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if slices.Index(order, "recovery") < 0 {
		t.Error("SetRecoveryActions was never called")
	}
	if slices.Index(order, "nonCrashRecovery:true") < 0 {
		t.Error("SetRecoveryActionsOnNonCrashFailures(true) was never called")
	}
}

func TestScmInstallRunsIcaclsSequence(t *testing.T) {
	var order []string
	m := &fakeMgr{calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, want := range icaclsHomeArgs(home.SystemHomeDir, testOperator) {
		if !exactCall(r.calls, want...) {
			t.Errorf("missing icacls step: %v", want)
		}
	}
}

func TestScmInstallReinstallUpdatesExistingService(t *testing.T) {
	var order []string
	existing := &fakeService{name: WindowsServiceName, calls: &order, status: svc.Status{State: svc.Running}}
	m := &fakeMgr{existing: existing, calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if slices.Index(order, "create:"+WindowsServiceName) >= 0 {
		t.Error("an existing service must not be recreated")
	}
	if slices.Index(order, "update:"+WindowsServiceName) < 0 {
		t.Error("an existing service's config must be updated (idempotent reinstall)")
	}
}

// Regression test: UpdateConfig (unlike CreateService, which quotes its
// exepath argument itself) passes BinaryPathName straight through with no
// escaping — an unquoted path containing a space parses ambiguously (the
// classic unquoted-service-path issue). A reinstall over an existing service
// must quote it the same way CreateService already does internally.
func TestScmInstallQuotesBinaryPathOnUpdate(t *testing.T) {
	var order []string
	existing := &fakeService{name: WindowsServiceName, calls: &order, status: svc.Status{State: svc.Stopped}}
	m := &fakeMgr{existing: existing, calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	inst.cfg.RunnydPath = `C:\Program Files\Runny\runnyd.exe`
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := `"C:\Program Files\Runny\runnyd.exe"`
	if existing.lastConfig.BinaryPathName != want {
		t.Errorf("BinaryPathName = %q, want %q", existing.lastConfig.BinaryPathName, want)
	}
}

// Regression test: Install used to call Start() unconditionally, which the
// real SCM rejects with ERROR_SERVICE_ALREADY_RUNNING for a service the
// reinstall found already running — silently failing the documented
// idempotent-reinstall path (and never applying the updated BinaryPathName,
// since that only takes effect on the next start). Install must stop a
// running service before starting it again.
func TestScmInstallReinstallOverRunningServiceStopsBeforeStarting(t *testing.T) {
	var order []string
	existing := &fakeService{name: WindowsServiceName, calls: &order, status: svc.Status{State: svc.Running}}
	m := &fakeMgr{existing: existing, calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	stopIdx := slices.Index(order, "stop")
	startIdx := slices.Index(order, "start")
	if stopIdx < 0 {
		t.Fatal("a running service was never stopped before restart")
	}
	if startIdx < 0 {
		t.Fatal("service was never started")
	}
	if stopIdx > startIdx {
		t.Errorf("service must be stopped (idx %d) BEFORE it is started again (idx %d)", stopIdx, startIdx)
	}
}

func TestScmUninstallStopsAndDeletes(t *testing.T) {
	var order []string
	existing := &fakeService{name: WindowsServiceName, calls: &order, status: svc.Status{State: svc.Running}}
	m := &fakeMgr{existing: existing, calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	var removedPath string
	inst.removeAll = func(path string) error { removedPath = path; return nil }
	if err := inst.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if slices.Index(order, "stop") < 0 {
		t.Error("a running service must be stopped")
	}
	if slices.Index(order, "delete") < 0 {
		t.Error("the service must be deleted after it stops")
	}
	if removedPath != home.SystemHomeDir {
		t.Errorf("removeAll called with %q, want %q", removedPath, home.SystemHomeDir)
	}
}

func TestScmUninstallRefusesStuckService(t *testing.T) {
	var order []string
	stuck := &fakeService{name: WindowsServiceName, calls: &order, status: svc.Status{State: svc.Running}, neverStops: true}
	m := &fakeMgr{existing: stuck, calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	// waitStopped bounds its poll at commandTimeout via context.WithTimeout —
	// a parent context with a short deadline wins the min(), so this test
	// doesn't wait out the real 30s ceiling.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := inst.Uninstall(ctx); err == nil {
		t.Fatal("Uninstall must refuse a service that never reports Stopped")
	}
	if slices.Index(order, "delete") >= 0 {
		t.Error("must not delete a service that never confirmed it stopped")
	}
}

// A service in a transitional state (StartPending etc.) can reject
// Control(Stop) with ERROR_SERVICE_CANNOT_ACCEPT_CTRL instead of stopping —
// waitStopped must retry rather than treat that as fatal.
func TestScmUninstallRetriesThroughTransitionalState(t *testing.T) {
	var order []string
	existing := &fakeService{name: WindowsServiceName, calls: &order, status: svc.Status{State: svc.Running}, settleAfter: 1}
	m := &fakeMgr{existing: existing, calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	if err := inst.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall must converge once the transitional state settles: %v", err)
	}
	if slices.Index(order, "delete") < 0 {
		t.Error("service must be deleted once it stops")
	}
}

func TestScmUninstallToleratesMissingService(t *testing.T) {
	var order []string
	m := &fakeMgr{existing: nil, calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	if err := inst.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall of a never-installed service must be a no-op success: %v", err)
	}
}
