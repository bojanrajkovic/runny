package vm

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bojanrajkovic/runny/internal/tart"
)

// asifMinMajor is the macOS major version Virtualization.framework needs to
// attach an ASIF disk. Below it, whether the attach fails cleanly or the
// guest hangs until BOOT's own deadline is unconfirmed either way, so
// checkASIFHost refuses upfront rather than finding out per boot.
const asifMinMajor = 26

// parseMacOSMajor parses a kern.osproductversion-shaped string ("26.0",
// "15.6.1", or a bare "26") into its major component. Only the major version
// distinguishes an ASIF-capable host, so the minor/patch components, if
// present, are ignored.
func parseMacOSMajor(version string) (int, error) {
	version = strings.TrimSpace(version)
	major, _, _ := strings.Cut(version, ".")
	if major == "" {
		return 0, fmt.Errorf("empty macOS version string")
	}
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, fmt.Errorf("parsing macOS version %q: %w", version, err)
	}
	return n, nil
}

// checkASIFHost refuses an ASIF bundle on a host below asifMinMajor, naming
// the host's actual version. It wraps tart.ErrASIFUnsupportedOnHost so this
// refusal and Hyper-V's outright one (internal/images, internal/oci) share
// one sentinel a caller can errors.Is against.
func checkASIFHost(version string) error {
	major, err := parseMacOSMajor(version)
	if err != nil {
		return fmt.Errorf("checking host macOS version for ASIF support: %w", err)
	}
	if major < asifMinMajor {
		return fmt.Errorf("%w: ASIF disk images need a macOS %d+ host (this host is %s)", tart.ErrASIFUnsupportedOnHost, asifMinMajor, version)
	}
	return nil
}
