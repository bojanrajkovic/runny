package main

import (
	"context"
	"fmt"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

func (c *ctl) operatorGrant(ctx context.Context, user string) error {
	resp, err := c.client.GrantOperator(ctx, &runnyv1.GrantOperatorRequest{User: user})
	if err != nil {
		return err
	}
	if c.json {
		return c.emit(resp)
	}
	fmt.Fprintf(c.out, "granted %s (uid %d) — reachable on the control socket now\n", resp.GetUser(), resp.GetUid())
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
	fmt.Fprintf(c.out, "revoked %s (uid %d) — new connections refused immediately\n", resp.GetUser(), resp.GetUid())
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
	fmt.Fprintf(c.out, "%-10s %-5s %-12s %s\n", "USER", "UID", "GRANTED BY", "WHEN")
	for _, op := range ops {
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
		fmt.Fprintf(c.out, "%-10s %-5d %-12s %s\n", op.GetUser(), op.GetUid(), grantedBy, when)
	}
	return nil
}
