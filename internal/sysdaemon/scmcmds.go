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

// icaclsHomeArgs is the home ACL reset scmInstaller.Install runs, in order.
// The shape rests on one measured platform fact: windows keeps a
// NON-protected child in sync with its parent's inheritable entries
// automatically, for additions AND removals. So the home directory is the only
// object that ever needs an entry written to it, and children inherit —
// including a later `operator revoke`, whose removal propagates down with no
// walk. That is the opposite of darwin, where inheritance is copy-at-create and
// the operator entry must therefore NOT inherit (internal/opacl).
//
// The corollary is that nothing below the home may be protected. A protected
// child stops tracking the parent in both directions — which is what an earlier
// /inheritance:d /T did, leaving `logs\` (created before this sequence runs)
// with no service-account entry at all on a fresh install, and the daemon
// unable to open its own log.
//
// /setowner operator runs first and reclaims ownership from whatever a PRIOR
// install left it as — the sequence's own last step transfers ownership to
// the service SID, so on a reinstall the elevated operator no longer owns
// the tree and would otherwise lack the WRITE_DAC the rest of this sequence
// needs. It works via SeTakeOwnershipPrivilege — a privilege every local
// Administrator holds — independent of the object's current DACL, which is
// exactly the recovery path it exists for. Ownership is not inherited, so this
// one keeps /T.
//
// /inheritance:d disables inheritance from ProgramData on the HOME while
// CONVERTING its entries to explicit ones rather than discarding them. It is
// deliberately not the more obvious /reset + /inheritance:r pair, which looks
// equivalent but isn't: confirmed against real hardware, that pair reproducibly
// leaves a freshly created two-level tree with an EMPTY DACL — denying everyone
// including the elevated caller — because stripping the root's inherited
// entries partway through a recursive operation makes the still-unprocessed
// child inaccessible before /inheritance:r reaches it.
//
// /remove:g then strips programDataLeakGroup from the home — the one entry the
// conversion keeps that actually matters, since ProgramData grants it read and
// that would leak the GitHub App key. Both /grant entries are (OI)(CI) so they
// reach every artifact written later, and Modify rather than Read because
// windows ownership confers no implicit access, unlike POSIX owner bits.
//
// The children's /reset runs LAST, against <home>\*, and the order is
// load-bearing: it makes every descendant drop its own explicit entries and
// inherit the home's — including a populated home from an older install, whose
// children each carry an explicit copy of the operator entry that no /remove:g
// against the home could ever have reached. Running it after the home is
// already clean is what keeps the App key from being momentarily readable by
// programDataLeakGroup, which a reset against the still-dirty home would
// reintroduce for the length of the sequence. It relies on the home always
// having at least one child when this runs; ensureHome creates logs\ first.
// The wildcard is spelled with a literal backslash rather than filepath.Join:
// this file carries no build tag, so on a darwin toolchain Join yields a
// forward slash and the pinned argument would silently diverge from what ships.
//
// Trade-off accepted deliberately: this does NOT wipe an explicit grant left on
// the HOME by a DIFFERENT prior --operator (the children's reset does clear
// theirs). That grant is real but not meaningfully exploitable on its own —
// running install-daemon at all requires local Administrator, and an admin
// already has file access to anything on the box regardless of this ACL. The
// one case it is not a no-op: a prior operator later demoted from Administrator
// elsewhere would still carry a stale Modify grant on the home directory.
func icaclsHomeArgs(homeDir, operator string) [][]string {
	return [][]string{
		{"icacls", homeDir, "/setowner", operator, "/T"},
		{"icacls", homeDir, "/inheritance:d"},
		{"icacls", homeDir, "/remove:g", programDataLeakGroup},
		{"icacls", homeDir, "/grant", windowsServiceSID + ":(OI)(CI)M"},
		{"icacls", homeDir, "/grant", operator + ":(OI)(CI)M"},
		{"icacls", homeDir + `\*`, "/reset", "/T"},
		{"icacls", homeDir, "/setowner", windowsServiceSID, "/T"},
	}
}
