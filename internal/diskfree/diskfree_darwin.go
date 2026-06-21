//go:build darwin

// Package diskfree reports available disk space. On Darwin it uses
// kCFURLVolumeAvailableCapacityForImportantUsageKey (CoreFoundation), which
// includes purgeable APFS space (local Time Machine snapshots, system caches)
// that macOS will reclaim before returning ENOSPC — the only API that correctly
// answers "will an 80-150 GiB write succeed?" on a typical macOS host. The key
// is available via pure C/CoreFoundation (no Objective-C required).
package diskfree

/*
#cgo LDFLAGS: -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <string.h>

long long available_bytes_for_important_usage(const char *path) {
	CFURLRef url = CFURLCreateFromFileSystemRepresentation(
		NULL, (const UInt8 *)path, (CFIndex)strlen(path), false);
	if (!url) return -1;

	CFTypeRef value = NULL;
	Boolean ok = CFURLCopyResourcePropertyForKey(
		url, kCFURLVolumeAvailableCapacityForImportantUsageKey, &value, NULL);
	CFRelease(url);
	if (!ok || !value) return -1;

	long long result = -1;
	if (CFGetTypeID(value) == CFNumberGetTypeID()) {
		CFNumberGetValue((CFNumberRef)value, kCFNumberLongLongType, &result);
	}
	CFRelease(value);
	return result;
}
*/
import "C"
import (
	"fmt"
	"unsafe"
)

// AvailableBytes returns the number of bytes available on the volume holding
// path, including purgeable space macOS will reclaim under write pressure.
func AvailableBytes(path string) (uint64, error) {
	cs := C.CString(path)
	defer C.free(unsafe.Pointer(cs))
	n := C.available_bytes_for_important_usage(cs)
	if n < 0 {
		return 0, fmt.Errorf("kCFURLVolumeAvailableCapacityForImportantUsageKey query failed for %q", path)
	}
	return uint64(n), nil
}
