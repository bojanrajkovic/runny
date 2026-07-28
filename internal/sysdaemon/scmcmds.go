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

// programDataLeakGroup is the principal ProgramData's own default DACL grants
// read (and, via a second ACE, limited write) to every local user — exactly
// the grant icaclsHomeArgs exists to strip before anything is staged into the
// home, so a freshly created (or freshly reinstalled) tree is never
// world-readable even for the brief window before /grant runs. Given as the
// well-known SID (S-1-5-32-545, BUILTIN\Users) rather than the display name:
// icacls resolves a bare name through the OS's own locale-dependent principal
// lookup, so a non-English-locale host could fail to match this on
// /remove:g — the `*SID` form icacls also accepts sidesteps that entirely.
const programDataLeakGroup = `*S-1-5-32-545`

// icaclsHomeArgs is the home ACL reset scmInstaller.Install runs, in order:
//
// /setowner operator runs first and reclaims ownership from whatever a PRIOR
// install left it as — the sequence's own last step transfers ownership to
// the service SID, so on a reinstall the elevated operator no longer owns
// the tree and would otherwise lack the WRITE_DAC the rest of this sequence
// needs. icacls's /setowner works via SeTakeOwnershipPrivilege — a privilege
// every local Administrator holds — independent of the object's current
// DACL, which is exactly the recovery path it exists for.
//
// /inheritance:d disables inheritance from ProgramData while CONVERTING the
// tree's current entries to explicit ones, rather than discarding them —
// this is deliberately not the more obvious /reset + /inheritance:r pair
// (reset the ACL to pure-inherited, then strip inherited entries), which
// looks equivalent but isn't: confirmed against real hardware, that pair
// reproducibly leaves a freshly created two-level tree (home + logs\) with
// an EMPTY DACL after /inheritance:r — denying access to everyone, including
// the elevated caller — because stripping the root's inherited entries to
// nothing partway through a recursive operation makes the still-unprocessed
// child inaccessible before /inheritance:r ever reaches it. This isn't a
// timing race (retried up to 30 times over 30s, same result every time) —
// it reproduces standalone, outside runnyd entirely, with no subsequent
// /grant call ever running to repopulate the now-empty ACL. /inheritance:d
// never produces that empty-DACL window: whatever was there (inherited or
// explicit) stays present throughout, just re-flagged.
//
// Neither /grant carries /T, and they differ in shape. The service gets
// (OI)(CI) so every artifact it later writes is reachable; windows re-propagates
// an inheritable entry to children that already exist, so /T would only add
// redundant explicit copies. The operator gets plain Modify on the home
// DIRECTORY — enough to rename a config edit into place, which windows charges
// to FILE_DELETE_CHILD on the parent — and nothing inheritable at all: a copy of
// the operator entry on a descendant is one no later /remove:g against the home
// could ever reach, which is exactly how a revoked operator used to keep access.
//
// /remove:g then strips programDataLeakGroup specifically — the one entry
// /inheritance:d's conversion doesn't get rid of on its own and that
// actually matters (ProgramData's default grants it read, which would leak
// the GitHub App key). /grant gives the service and operator principals
// Modify — not Read; Windows ownership confers no implicit access, unlike
// POSIX owner bits — and the final /setowner hands ownership back to the
// service SID, the signal home.ResolveServer keys on.
//
// Trade-off accepted deliberately: unlike the old /reset, this sequence does
// NOT wipe an explicit grant left by a DIFFERENT prior --operator on an
// earlier install. That grant is real but not meaningfully exploitable on
// its own — running install-daemon at all requires local Administrator
// (requireInstallPrivilege), and an admin already has file access to
// anything on the box regardless of this ACL (SeTakeOwnershipPrivilege lets
// them reclaim it same as this code does). The one case it's NOT a no-op:
// a prior operator later demoted from Administrator elsewhere would still
// carry this stale Modify grant as genuine residual access. Narrow enough,
// and cheap enough to fix later (an explicit /remove:g for a
// previously-recorded operator, if one ever needs tracking) that it isn't
// worth reintroducing the /reset-shaped empty-DACL hazard to close now.
func icaclsHomeArgs(homeDir, operator string) [][]string {
	return [][]string{
		{"icacls", homeDir, "/setowner", operator, "/T"},
		{"icacls", homeDir, "/inheritance:d", "/T"},
		{"icacls", homeDir, "/remove:g", programDataLeakGroup, "/T"},
		{"icacls", homeDir, "/grant", windowsServiceSID + ":(OI)(CI)M"},
		{"icacls", homeDir, "/grant", operator + ":M"},
		{"icacls", homeDir, "/setowner", windowsServiceSID, "/T"},
	}
}
