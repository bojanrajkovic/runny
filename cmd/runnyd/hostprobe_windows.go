//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// memoryStatusEx mirrors MEMORYSTATUSEX (sysinfoapi.h) -- x/sys/windows has
// no binding for GlobalMemoryStatusEx, so this is a small hand-written one,
// laid out directly from the struct docs:
// https://learn.microsoft.com/windows/win32/api/sysinfoapi/ns-sysinfoapi-memorystatusex
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

var (
	modkernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = modkernel32.NewProc("GlobalMemoryStatusEx")
)

// physicalRAMGB returns the host's physical RAM in GiB via GlobalMemoryStatusEx,
// or 0 if it can't be read -- 0 disables the RAM overcommit axis rather than
// guessing.
func physicalRAMGB() uint {
	m := memoryStatusEx{length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	ret, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if ret == 0 {
		return 0
	}
	return uint(m.totalPhys >> 30)
}
