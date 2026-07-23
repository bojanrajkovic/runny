package main

import (
	"context"
	"fmt"
	"strconv"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// operatorIDDisplay renders a mutation/list entry's platform-native
// identity: darwin's numeric shape keeps its "uid N" wording; a Windows SID
// renders verbatim (the daemon already resolved the account name beside it,
// so the SID is the identity, not a puzzle for the reader).
func operatorIDDisplay(uid uint32, sid string) string {
	if sid != "" {
		return sid
	}
	return fmt.Sprintf("uid %d", uid)
}

func (c *ctl) operatorGrant(ctx context.Context, user string) error {
	resp, err := c.client.GrantOperator(ctx, &runnyv1.GrantOperatorRequest{User: user})
	if err != nil {
		return err
	}
	if c.json {
		return c.emit(resp)
	}
	fmt.Fprintf(c.out, "granted %s (%s) — reachable on the control channel now\n",
		resp.GetUser(), operatorIDDisplay(resp.GetUid(), resp.GetSid()))
	return nil
}

func (c *ctl) operatorRevoke(ctx context.Context, user string) error {
	resp, err := c.client.RevokeOperator(ctx, &runnyv1.RevokeOperatorRequest{User: user})
	if err != nil {
		return err
	}
	if c.json {
		return c.emit(resp)
	}
	fmt.Fprintf(c.out, "revoked %s (%s) — new connections refused immediately\n",
		resp.GetUser(), operatorIDDisplay(resp.GetUid(), resp.GetSid()))
	return nil
}

func (c *ctl) operatorList(ctx context.Context) error {
	resp, err := c.client.ListOperators(ctx, &runnyv1.ListOperatorsRequest{})
	if err != nil {
		return err
	}
	if c.json {
		return c.emit(resp)
	}
	ops := resp.GetOperators()
	if len(ops) == 0 {
		fmt.Fprintln(c.out, "no operators granted")
		return nil
	}
	fmt.Fprintf(c.out, "%-10s %-5s %-12s %s\n", "USER", "ID", "GRANTED BY", "WHEN")
	for _, op := range ops {
		// The identity column carries whichever shape the daemon minted: a
		// decimal uid (darwin) or the full SID (Windows) — no truncation, an
		// audit surface reads exactly.
		id := op.GetSid()
		if id == "" {
			id = strconv.FormatUint(uint64(op.GetUid()), 10)
		}
		grantedBy := op.GetGrantedBy()
		when := ""
		if at := op.GetGrantedAt(); at != nil {
			when = at.AsTime().Local().Format("2006-01-02")
		}
		if grantedBy == "" {
			// No timestamp either: no grant record exists at all — the
			// install-time bootstrap operator. A timestamp WITH no name is a
			// real RPC grant whose peer-identity read failed (fail-open, see
			// operatorIdentity) — that must keep its "when", not collapse
			// into the bootstrap label.
			if when == "" {
				grantedBy = "(install)"
			} else {
				grantedBy = "(unknown)"
			}
		}
		fmt.Fprintf(c.out, "%-10s %-5s %-12s %s\n", op.GetUser(), id, grantedBy, when)
	}
	return nil
}
