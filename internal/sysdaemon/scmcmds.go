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
// strip inherited ProgramData Users-read (it would leak the GitHub App key),
// grant the service and operator principals Modify — not Read; Windows
// ownership confers no implicit access, unlike POSIX owner bits — then hand
// ownership to the service SID, the signal home.ResolveServer keys on.
func icaclsHomeArgs(homeDir, operator string) [][]string {
	return [][]string{
		{"icacls", homeDir, "/inheritance:r"},
		{"icacls", homeDir, "/grant", windowsServiceSID + ":(OI)(CI)M", "/T"},
		{"icacls", homeDir, "/grant", operator + ":(OI)(CI)M", "/T"},
		{"icacls", homeDir, "/setowner", windowsServiceSID, "/T"},
	}
}
