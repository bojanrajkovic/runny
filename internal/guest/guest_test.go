package guest

import (
	"strings"
	"testing"
)

// The darwin runner launches over a non-login SSH exec, whose PATH lacks
// /usr/local/bin; the provision script must rebuild a login PATH so job steps
// find tools that pkg installers symlink there. Regression guard for the
// "aws: command not found" right after a successful install.
func TestProvisionScriptDarwinPrimesPATH(t *testing.T) {
	if !strings.Contains(provisionScriptDarwin, "/usr/libexec/path_helper") {
		t.Error("darwin provision script must rebuild PATH via path_helper before launching the runner")
	}
}
