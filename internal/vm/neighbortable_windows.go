//go:build windows

package vm

import (
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WaitIP's host-side read: HNS pre-commits a guest's MAC->IP binding into the
// host IP neighbor table at endpoint attach (validated on real hardware,
// issue #307/#308) -- state Permanent, present within seconds of Start, gone
// only once the HNS endpoint backing it is deleted. x/sys/windows has no
// binding for this corner of iphlpapi, so this file is a small hand-written
// one, laid out directly from the authoritative struct docs (cited per
// field below) rather than generated -- the same spec-traceability
// discipline internal/vhdx uses for [MS-VHDX] offsets.

var (
	modiphlpapi           = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetIpNetTable2    = modiphlpapi.NewProc("GetIpNetTable2")
	procFreeMibTable      = modiphlpapi.NewProc("FreeMibTable")
	procDeleteIpNetEntry2 = modiphlpapi.NewProc("DeleteIpNetEntry2")
)

const (
	afINET = 2 // AF_INET, per ws2def.h -- the only family this backend queries.

	// ifMaxPhysAddressLength is IF_MAX_PHYS_ADDRESS_LENGTH (ifdef.h): the
	// fixed capacity of MIB_IPNET_ROW2.PhysicalAddress, not the length of
	// any one address (that's PhysicalAddressLength).
	ifMaxPhysAddressLength = 32
)

// The NL_NEIGHBOR_STATE enum, neighborEntry, and the pure row-selection logic
// (permanentIPs/selectLeaseIP) live in neighbortable.go, untagged, so
// they're testable off real hardware -- this file is the windows-only
// GetIpNetTable2 syscall glue that produces the rows they select from.

// sockaddrInet mirrors SOCKADDR_INET (ws2ipdef.h):
// https://learn.microsoft.com/windows/win32/api/ws2ipdef/ns-ws2ipdef-sockaddr_inet
// a union of SOCKADDR_IN (16 bytes) and SOCKADDR_IN6 (28 bytes) -- 28 bytes
// covers either. Represented as trailing uint32 words (not a byte array) so
// this type's own alignment is 4, matching the union's real alignment (from
// SOCKADDR_IN6's ULONG sin6_flowinfo) -- getting that wrong would silently
// misplace every field laid out after it in mibIPNetRow2. This backend only
// ever populates/reads the IPv4 view (AF_INET is the only family queried).
type sockaddrInet struct {
	family uint16
	port   uint16
	addr4  [4]byte
	_      [5]uint32 // rest of the SOCKADDR_IN6 arm, unused for AF_INET
}

// mibIPNetRow2 mirrors MIB_IPNET_ROW2 (netioapi.h), field-for-field and
// padding-for-padding, per:
// https://learn.microsoft.com/windows/win32/api/netioapi/ns-netioapi-mib_ipnet_row2
// The trailing 3-byte gap exists because flags is 1 byte but
// reachabilityTime (a ULONG union) needs 4-byte alignment; Microsoft's own
// docs warn the real table may pad between entries for exactly this reason,
// so this layout is not guessed -- it's the struct as documented.
type mibIPNetRow2 struct {
	address               sockaddrInet
	interfaceIndex        uint32
	interfaceLuid         uint64
	physicalAddress       [ifMaxPhysAddressLength]byte
	physicalAddressLength uint32
	state                 int32
	flags                 byte
	_                     [3]byte
	reachabilityTime      uint32
}

// mibIPNetTable2Header mirrors MIB_IPNET_TABLE2's leading NumEntries field;
// the row array follows immediately (Microsoft's docs note possible
// alignment padding between NumEntries and the first row, but NumEntries is
// a ULONG and mibIPNetRow2's own alignment is 8 (from interfaceLuid), so on
// amd64 that padding is 4 bytes -- accounted for via the offset arithmetic
// in neighborEntries, not folded into this header type, since Go can't
// express "N bytes of padding then start the flexible array" as struct
// fields the way C's trailing array does.
type mibIPNetTable2Header struct {
	numEntries uint32
}

// readNeighborEntries is neighborEntries, swappable in tests so WaitIP's poll
// loop (hcs_windows.go) exercises its matching logic against a fake table
// instead of a live syscall -- same seam shape as stop.go's stopSettle var.
var readNeighborEntries = neighborEntries

// neighborEntries calls GetIpNetTable2(AF_INET, ...), copies out every row
// as a neighborEntry, and frees the table -- the caller never sees the raw
// pointer, so there is no way to leak it or read it after FreeMibTable.
func neighborEntries() ([]neighborEntry, error) {
	var table *byte
	r, _, _ := procGetIpNetTable2.Call(uintptr(afINET), uintptr(unsafe.Pointer(&table)))
	if r != 0 {
		// ERROR_NOT_FOUND (per Microsoft's docs) means "call succeeded, table
		// is just empty" -- not a failure, and callers (a poll loop) need to
		// keep polling rather than treat this as terminal.
		if windows.Errno(r) == windows.ERROR_NOT_FOUND {
			return nil, nil
		}
		return nil, fmt.Errorf("GetIpNetTable2: %w", windows.Errno(r))
	}
	defer procFreeMibTable.Call(uintptr(unsafe.Pointer(table)))

	header := (*mibIPNetTable2Header)(unsafe.Pointer(table))
	// The row array starts after NumEntries, aligned to mibIPNetRow2's own
	// 8-byte alignment (see mibIPNetTable2Header's comment). unsafe.Add
	// (not uintptr arithmetic converted back to Pointer) is the vet-clean
	// idiom for offsetting into a block the Go GC doesn't manage.
	rowsStart := unsafe.Add(unsafe.Pointer(table), unsafe.Sizeof(mibIPNetTable2Header{}))
	if rem := uintptr(rowsStart) % unsafe.Alignof(mibIPNetRow2{}); rem != 0 {
		rowsStart = unsafe.Add(rowsStart, unsafe.Alignof(mibIPNetRow2{})-rem)
	}
	rowSize := unsafe.Sizeof(mibIPNetRow2{})

	entries := make([]neighborEntry, 0, header.numEntries)
	for i := uint32(0); i < header.numEntries; i++ {
		row := (*mibIPNetRow2)(unsafe.Add(rowsStart, uintptr(i)*rowSize))
		if row.address.family != afINET {
			continue // this backend only ever attaches IPv4 endpoints
		}
		entries = append(entries, neighborEntry{
			ip:             net.IP(row.address.addr4[:]).String(),
			mac:            formatPhysicalAddress(row.physicalAddress, row.physicalAddressLength),
			state:          row.state,
			interfaceIndex: row.interfaceIndex,
		})
	}
	return entries, nil
}

// formatPhysicalAddress renders a MIB_IPNET_ROW2.PhysicalAddress as
// colon-separated hex via net.HardwareAddr -- length is the row's own
// PhysicalAddressLength; the fixed-size array is padding beyond it.
func formatPhysicalAddress(addr [ifMaxPhysAddressLength]byte, length uint32) string {
	if length == 0 || length > ifMaxPhysAddressLength {
		return ""
	}
	return net.HardwareAddr(addr[:length]).String()
}

// deleteNeighborEntry issues DeleteIpNetEntry2 for the exact row neighborIP
// on the given host interface -- used at Machine teardown to scrub the
// Permanent entry HNS leaves behind, which this backend's own hardware spike
// found does NOT clear itself on endpoint delete (see hcs_windows.go's
// teardown). interfaceIndex must come from a neighborEntries() row already
// matched by MAC; per Microsoft's docs, only Address plus one of
// InterfaceLuid/InterfaceIndex need be set, and leaving InterfaceLuid zero
// makes InterfaceIndex the one that's used.
func deleteNeighborEntry(neighborIP string, interfaceIndex uint32) error {
	ip, err := parseIPv4(neighborIP)
	if err != nil {
		return err
	}
	row := mibIPNetRow2{
		address:        sockaddrInet{family: afINET, addr4: ip},
		interfaceIndex: interfaceIndex,
	}
	r, _, _ := procDeleteIpNetEntry2.Call(uintptr(unsafe.Pointer(&row)))
	if r != 0 {
		// ERROR_NOT_FOUND means it's already gone -- not this caller's problem.
		if windows.Errno(r) == windows.ERROR_NOT_FOUND {
			return nil
		}
		return fmt.Errorf("DeleteIpNetEntry2: %w", windows.Errno(r))
	}
	return nil
}

func parseIPv4(s string) ([4]byte, error) {
	ip := net.ParseIP(s).To4()
	if ip == nil {
		return [4]byte{}, fmt.Errorf("not a dotted-quad IPv4 address: %q", s)
	}
	return [4]byte(ip), nil
}
