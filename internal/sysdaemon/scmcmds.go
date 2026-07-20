package sysdaemon

// WindowsServiceName is the SCM service name runnyd registers under
// (cmd/runnyd/svc_windows.go's svc.Run("runnyd", h)) and the installer creates
// under — the two must agree, or the installed service and the running
// process are strangers to each other.
const WindowsServiceName = "runnyd"

// windowsDisplayName is the service's human-readable name in the Services
// console and `sc.exe query`.
const windowsDisplayName = "Runny Daemon"

// windowsServiceSID is the virtual-account principal both icacls and
// mgr.Config.ServiceStartName address the service by. It only resolves once
// the service is registered (LookupSID fails before CreateService runs) — see
// scmInstaller.Install's ordering.
const windowsServiceSID = `NT SERVICE\` + WindowsServiceName

// icaclsHomeArgs is the home ACL reset scmInstaller.Install runs, in order:
// /reset discards every explicit ACE (darwin's equivalent is ensureHome's
// "chmod -R -N") — without it, a reinstall under a DIFFERENT --operator would
// leave the previous operator's Modify grant in place, since /inheritance:r
// only strips INHERITED entries and /grant only adds, neither removes a
// stale explicit grant for a principal no longer being installed for. Then
// /inheritance:r strips the inherited ProgramData Users-read /reset just
// restored (it would leak the GitHub App key) — /T is load-bearing here too:
// ensureHome creates logs\ before this runs, and a non-recursive
// /inheritance:r only strips the home ROOT's inherited entries, leaving
// logs\ (and, on a reinstall, any other pre-existing subdirectory) with the
// inherited world-read ACE the whole sequence exists to remove. /grant then
// gives the service and operator principals Modify — not Read; Windows
// ownership confers no implicit access, unlike POSIX owner bits — and
// /setowner hands ownership to the service SID, the signal
// home.ResolveServer keys on.
//
// ponytail: /reset and /inheritance:r are separate icacls.exe processes, not
// one atomic operation — on a reinstall, an already-present local user could
// theoretically race the interval between them and read a file the
// permissive ProgramData-inherited ACL /reset just restored. Accepted: this
// targets a dedicated headless CI/build host (docs/deploy.md), not a shared
// multi-user workstation, and the race needs an already-present, already-
// malicious local account with sub-second timing. Not worth an atomic
// replacement DACL for that threat model; revisit if this ever runs
// somewhere multi-tenant.
func icaclsHomeArgs(homeDir, operator string) [][]string {
	return [][]string{
		{"icacls", homeDir, "/reset", "/T"},
		{"icacls", homeDir, "/inheritance:r", "/T"},
		{"icacls", homeDir, "/grant", windowsServiceSID + ":(OI)(CI)M", "/T"},
		{"icacls", homeDir, "/grant", operator + ":(OI)(CI)M", "/T"},
		{"icacls", homeDir, "/setowner", windowsServiceSID, "/T"},
	}
}
