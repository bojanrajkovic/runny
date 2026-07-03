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
	"os/user"
	"strconv"
	"strings"
	"unsafe"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// maxACLOperators bounds the ALLOW-user entries List reads back from one
// ACL: a bounded stack buffer, not a per-daemon operator-count limit anyone
// is expected to approach.
const maxACLOperators = 64

// ListUIDs reads homeDir's ACL for ALLOW-user-with-write entries and returns
// the raw uids, skipping the per-entry user.LookupId resolution List does —
// that resolution is display-only and the sole NSS-backed (potentially
// slow) part of List. The per-RPC revocation gate calls this on every
// RPC-start, so it must never pay directory-service latency.
func ListUIDs(homeDir string) ([]uint32, error) {
	cpath := C.CString(homeDir)
	defer C.free(unsafe.Pointer(cpath))
	var buf [maxACLOperators]C.uint32_t
	n := int(C.read_acl_allow_uids(cpath, &buf[0], C.int(len(buf))))
	if n < 0 {
		return nil, nil // no ACL, or unreadable: no operators
	}
	uids := make([]uint32, n)
	for i := 0; i < n; i++ {
		uids[i] = uint32(buf[i])
	}
	return uids, nil
}

// List reads homeDir's ACL for ALLOW-user entries — the authoritative,
// durable operator set — via ListUIDs, resolving each uid to a username
// best-effort (an unresolvable uid still appears, with an empty User).
// Validated against a real chmod +a grant by the aclprobe spike.
func List(homeDir string) ([]Operator, error) {
	uids, err := ListUIDs(homeDir)
	if err != nil {
		return nil, err
	}
	ops := make([]Operator, 0, len(uids))
	for _, uid := range uids {
		name := ""
		if u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10)); err == nil {
			name = u.Username
		}
		ops = append(ops, Operator{UID: uid, User: name})
	}
	return ops, nil
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
