//go:build !windows

package sysdaemon

import (
	"fmt"
	"os"

	"github.com/bojanrajkovic/runny/internal/testconfig"
)

// New builds an Installer that shells out for real and logs progress to stdout.
func New(cfg Config) *Installer {
	return &Installer{
		cfg:        cfg,
		run:        execRunner,
		writeFile:  os.WriteFile,
		log:        func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
		testConfig: testconfig.RunTestConfig,
	}
}
