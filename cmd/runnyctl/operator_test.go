package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

type fakeOperatorClient struct {
	runnyv1.RunnyServiceClient
	granted  *runnyv1.GrantOperatorRequest
	revoked  *runnyv1.RevokeOperatorRequest
	grantErr error
	revoked1 error
	list     *runnyv1.ListOperatorsResponse
	listErr  error
}

func (f *fakeOperatorClient) GrantOperator(_ context.Context, req *runnyv1.GrantOperatorRequest, _ ...grpc.CallOption) (*runnyv1.OperatorMutation, error) {
	f.granted = req
	if f.grantErr != nil {
		return nil, f.grantErr
	}
	return &runnyv1.OperatorMutation{Uid: 502, User: req.GetUser()}, nil
}

func (f *fakeOperatorClient) RevokeOperator(_ context.Context, req *runnyv1.RevokeOperatorRequest, _ ...grpc.CallOption) (*runnyv1.OperatorMutation, error) {
	f.revoked = req
	if f.revoked1 != nil {
		return nil, f.revoked1
	}
	return &runnyv1.OperatorMutation{Uid: 502, User: req.GetUser()}, nil
}

func (f *fakeOperatorClient) ListOperators(_ context.Context, _ *runnyv1.ListOperatorsRequest, _ ...grpc.CallOption) (*runnyv1.ListOperatorsResponse, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list, nil
}

func TestOperatorGrantRendersConfirmation(t *testing.T) {
	var buf bytes.Buffer
	f := &fakeOperatorClient{}
	c := &ctl{client: f, out: &buf}
	if err := c.operatorGrant(t.Context(), "alice"); err != nil {
		t.Fatalf("operatorGrant: %v", err)
	}
	if f.granted.GetUser() != "alice" {
		t.Errorf("request user = %q, want alice", f.granted.GetUser())
	}
	if got := buf.String(); got != "granted alice (uid 502) — reachable on the control socket now\n" {
		t.Errorf("output = %q", got)
	}
}

func TestOperatorGrantPropagatesError(t *testing.T) {
	f := &fakeOperatorClient{grantErr: status.Error(codes.FailedPrecondition, "alice is already an operator")}
	c := &ctl{client: f, out: &bytes.Buffer{}}
	err := c.operatorGrant(t.Context(), "alice")
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestOperatorRevokeRendersConfirmation(t *testing.T) {
	var buf bytes.Buffer
	f := &fakeOperatorClient{}
	c := &ctl{client: f, out: &buf}
	if err := c.operatorRevoke(t.Context(), "alice"); err != nil {
		t.Fatalf("operatorRevoke: %v", err)
	}
	if f.revoked.GetUser() != "alice" {
		t.Errorf("request user = %q, want alice", f.revoked.GetUser())
	}
	if got := buf.String(); got != "revoked alice (uid 502) — new connections refused immediately\n" {
		t.Errorf("output = %q", got)
	}
}

func TestOperatorListRendersTable(t *testing.T) {
	when := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	f := &fakeOperatorClient{list: &runnyv1.ListOperatorsResponse{
		Operators: []*runnyv1.Operator{
			{Uid: 501, User: "brajkovic"}, // install bootstrap: no attribution
			{Uid: 502, User: "alice", GrantedBy: "brajkovic", GrantedAt: timestamppb.New(when)},
		},
	}}
	var buf bytes.Buffer
	c := &ctl{client: f, out: &buf}
	if err := c.operatorList(t.Context()); err != nil {
		t.Fatalf("operatorList: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "brajkovic") || !strings.Contains(out, "(install)") {
		t.Errorf("bootstrap operator not rendered as (install):\n%s", out)
	}
	if !strings.Contains(out, "alice") || !strings.Contains(out, "2026-06-20") {
		t.Errorf("granted operator missing attribution:\n%s", out)
	}
}

func TestOperatorRevokePropagatesError(t *testing.T) {
	f := &fakeOperatorClient{revoked1: status.Error(codes.FailedPrecondition, "refusing to revoke the last operator")}
	c := &ctl{client: f, out: &bytes.Buffer{}}
	err := c.operatorRevoke(t.Context(), "alice")
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestOperatorListPropagatesError(t *testing.T) {
	f := &fakeOperatorClient{listErr: status.Error(codes.Internal, "reading the operator set")}
	c := &ctl{client: f, out: &bytes.Buffer{}}
	err := c.operatorList(t.Context())
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
}

func TestOperatorListEmpty(t *testing.T) {
	f := &fakeOperatorClient{list: &runnyv1.ListOperatorsResponse{}}
	var buf bytes.Buffer
	c := &ctl{client: f, out: &buf}
	if err := c.operatorList(t.Context()); err != nil {
		t.Fatalf("operatorList: %v", err)
	}
	if buf.String() != "no operators granted\n" {
		t.Errorf("output = %q", buf.String())
	}
}

func TestOperatorDispatchUnknownSubcommand(t *testing.T) {
	c := &ctl{client: &fakeOperatorClient{}, out: &bytes.Buffer{}}
	err := c.dispatch(t.Context(), []string{"operator", "frobnicate"})
	if err == nil {
		t.Fatal("expected an error for an unknown operator subcommand")
	}
}

func TestOperatorDispatchNoSubcommand(t *testing.T) {
	c := &ctl{client: &fakeOperatorClient{}, out: &bytes.Buffer{}}
	err := c.dispatch(t.Context(), []string{"operator"})
	if err == nil {
		t.Fatal("expected an error when operator is called with no subcommand")
	}
}
