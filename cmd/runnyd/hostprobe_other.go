//go:build !darwin

package main

// physicalRAMGB is unknown off darwin — runnyd's production host is always
// darwin; this build exists only for the pure-Go CI leg. 0 disables the RAM
// overcommit axis.
func physicalRAMGB() uint { return 0 }
