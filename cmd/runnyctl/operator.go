package main

import (
	"context"
	"fmt"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// operatorDispatch handles the "operator" command group — runnyctl's first
// subcommand group; grant/revoke/list are one cohesive noun, unlike the
// otherwise-flat command set.
func (c *ctl) operatorDispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("operator requires a subcommand: grant, revoke, or list")
	}
	switch verb, rest := args[0], args[1:]; verb {
	case "grant":
		fs, j := subFlags("operator grant")
		user, err := c.oneArg(fs, j, rest, "USER")
		if err != nil {
			return err
		}
		return c.operatorGrant(ctx, user)
	case "revoke":
		fs, j := subFlags("operator revoke")
		user, err := c.oneArg(fs, j, rest, "USER")
		if err != nil {
			return err
		}
		return c.operatorRevoke(ctx, user)
	case "list":
		fs, j := subFlags("operator list")
		if err := c.parseNoArgs(fs, j, rest); err != nil {
			return err
		}
		return c.operatorList(ctx)
	default:
		return fmt.Errorf("unknown operator subcommand %q (want grant, revoke, or list)", verb)
	}
}

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
