//go:build darwin

package opacl

/*
#include <sys/types.h>
#include <sys/acl.h>
#include <membership.h>
#include <stdint.h>
#include <stdlib.h>
#include <errno.h>

// read_acl_allow_uids reads ALLOW ACEs whose principal resolves to a uid AND
// whose permission set carries write. write is the operator-defining bit,
// not an arbitrary filter: it is what lets a principal connect() to the
// inherited socket (the aclprobe spike's claim C2). Without this check, the
// _runny service account's read-only ACE (sysdaemon's serviceACE, granted so
// the daemon can read operator-landed files) is indistinguishable from a
// real operator grant here, which would silently defeat the last-operator
// revoke guard on every real install.
// Returns the count written to out (<=max), or -1 if the path carries no ACL.
static int read_acl_allow_uids(const char *path, uint32_t *out, int max) {
    errno = 0;
    acl_t acl = acl_get_file(path, ACL_TYPE_EXTENDED);
    if (acl == NULL) {
        return -1; // no extended ACL, or unreadable (errno set)
    }
    int n = 0;
    acl_entry_t entry;
    for (int r = acl_get_entry(acl, ACL_FIRST_ENTRY, &entry);
         r == 0;
         r = acl_get_entry(acl, ACL_NEXT_ENTRY, &entry)) {
        acl_tag_t tag;
        if (acl_get_tag_type(entry, &tag) != 0 || tag != ACL_EXTENDED_ALLOW) {
            continue;
        }
        acl_permset_t permset;
        if (acl_get_permset(entry, &permset) != 0 || acl_get_perm_np(permset, ACL_WRITE_DATA) != 1) {
            continue;
        }
        void *q = acl_get_qualifier(entry); // guid_t* for user/group entries
        if (q == NULL) {
            continue;
        }
        uid_t id;
        int idtype;
        if (mbr_uuid_to_id((unsigned char *)q, &id, &idtype) == 0 && idtype == ID_TYPE_UID) {
            if (n < max) {
                out[n++] = (uint32_t)id;
            }
        }
        acl_free(q);
    }
    acl_free(acl);
    return n;
}
*/
import "C"

import (
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"unsafe"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// maxACLOperators bounds the ALLOW-user entries List/ListIDs reads back
// from one ACL: a bounded stack buffer, not a per-daemon operator-count
// limit anyone is expected to approach. Entries beyond this are silently
// dropped (read_acl_allow_uids has no overflow signal), which used to only
// affect List's display output but now also feeds ListIDs's per-RPC
// authorization check (internal/socket/revocation.go) — sized generously
// (healthy magnitude × a wide margin, not the sum of any real deployment's
// expected grant count) as a stopgap against that; a real fix would make
// truncation an explicit, loud error instead of a silent undercount.
const maxACLOperators = 1024

// ListIDs reads homeDir's ACL for ALLOW-user-with-write entries and returns
// the raw uids as decimal strings (os/user.User.Uid's platform-native
// convention), skipping the per-entry user.LookupId resolution List does —
// that resolution is display-only and the sole NSS-backed (potentially
// slow) part of List. The per-RPC revocation gate calls this on every
// RPC-start, so it must never pay directory-service latency.
func ListIDs(homeDir string) ([]string, error) {
	cpath := C.CString(homeDir)
	defer C.free(unsafe.Pointer(cpath))
	var buf [maxACLOperators]C.uint32_t
	n := int(C.read_acl_allow_uids(cpath, &buf[0], C.int(len(buf))))
	if n < 0 {
		return nil, nil // no ACL, or unreadable: no operators
	}
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = strconv.FormatUint(uint64(buf[i]), 10)
	}
	return ids, nil
}

// Grant adds an inheriting operator ACE for username to homeDir (so every
// subsequently created file and the NEXT socket inherit it) and directly to
// sock (the current live socket, which already exists and so does not pick
// up the dir's ACE by inheritance — inheritance is copy-at-create). Not
// recursive: a grant only reaches artifacts from this point forward: see
// docs/security.md's "no recursive re-stamp" sharp edge.
func Grant(ctx bounded.Context, homeDir, sock, username string) error {
	return chmodBoth(ctx, "+a", homeDir, sock, username)
}

// Revoke removes username's operator ACE from homeDir and sock.
func Revoke(ctx bounded.Context, homeDir, sock, username string) error {
	return chmodBoth(ctx, "-a", homeDir, sock, username)
}

// StampSocket gives every current operator write on sock. A freshly created
// socket carries no operator entry of its own: the operator entry lives on the
// home dir and does not inherit, and the socket is 0600 owned by the daemon
// (internal/socket's listen). The daemon re-creates the socket on every start,
// so without this a restart would lock every operator out of their own daemon.
//
// This is the darwin analogue of the windows control channel's security
// descriptor, which is likewise built from the operator set when the pipe is
// created rather than inherited from anything. The operator set is read from
// the home dir, which stays the single authority for who an operator is.
//
// An operator whose identity no longer resolves to a name is skipped: chmod
// addresses a principal by name, so there is nothing to stamp, and List already
// reports such an entry with an empty user.
func StampSocket(ctx bounded.Context, homeDir, sock string) error {
	ops, err := List(homeDir)
	if err != nil {
		return fmt.Errorf("reading %s's operator set: %w", homeDir, err)
	}
	for _, op := range ops {
		if op.User == "" {
			continue
		}
		if err := chmod(ctx, "+a", OperatorACE(op.User), sock); err != nil {
			return err
		}
	}
	return nil
}

func chmodBoth(ctx bounded.Context, verb, homeDir, sock, username string) error {
	ace := OperatorACE(username)
	if err := chmod(ctx, verb, ace, homeDir); err != nil {
		return err
	}
	return chmod(ctx, verb, ace, sock)
}

func chmod(ctx bounded.Context, verb, ace, path string) error {
	out, err := exec.CommandContext(ctx, "/bin/chmod", verb, ace, path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("chmod %s %q %s: %w: %s", verb, ace, path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// HasID reports whether id holds an operator ACE on homeDir. On darwin the ACL
// read yields raw uids with no name resolution involved, so there is no lookup
// that could fail and membership is simply the listing -- unlike windows,
// where the two paths must differ.
func HasID(homeDir, id string) (bool, error) {
	ids, err := ListIDs(homeDir)
	if err != nil {
		return false, err
	}
	return slices.Contains(ids, id), nil
}
