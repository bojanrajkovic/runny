//go:build windows

package sysdaemon

import (
	"fmt"
	"os"

	"github.com/bojanrajkovic/runny/internal/testconfig"
)

// New builds a scmInstaller that talks to the real service control manager
// and logs progress to stdout.
func New(cfg Config) *scmInstaller {
	return &scmInstaller{
		cfg:        cfg,
		connect:    connectSCM,
		run:        execRunner,
		writeFile:  os.WriteFile,
		mkdirAll:   os.MkdirAll,
		removeAll:  os.RemoveAll,
		log:        func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
		testConfig: testconfig.RunTestConfig,
	}
}
