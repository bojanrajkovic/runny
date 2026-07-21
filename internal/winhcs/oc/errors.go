package oc

import (
	"context"
	"errors"
	"io"
	"net"
	"os"

	"go.opencensus.io/trace"
)

// toStatusCode maps a Go sentinel error to an OpenCensus span status code
// (https://github.com/googleapis/googleapis/blob/master/google/rpc/code.proto,
// the same space gRPC uses). Upstream hcsshim delegates unmatched errors to
// containerd/errdefs' gRPC-status translation, which drags in
// containerd/errdefs, google.golang.org/grpc, and google.golang.org/protobuf
// -- none of which the vendored winhcs boot path needs, since it never
// produces containerd- or gRPC-flavored errors. This local mapper covers
// only the error classes the vendored hcs package actually produces.
func toStatusCode(err error) int {
	switch {
	case isAny(err, context.Canceled):
		return trace.StatusCodeCancelled
	case isAny(err, os.ErrInvalid):
		return trace.StatusCodeInvalidArgument
	case isAny(err, os.ErrDeadlineExceeded, context.DeadlineExceeded):
		return trace.StatusCodeDeadlineExceeded
	case isAny(err, os.ErrNotExist):
		return trace.StatusCodeNotFound
	case isAny(err, os.ErrExist):
		return trace.StatusCodeAlreadyExists
	case isAny(err, os.ErrPermission):
		return trace.StatusCodePermissionDenied
	case isAny(err, os.ErrClosed, net.ErrClosed, io.ErrClosedPipe, io.ErrShortBuffer):
		return trace.StatusCodeFailedPrecondition
	case isAny(err, io.ErrNoProgress):
		return trace.StatusCodeInternal
	case isAny(err, io.ErrShortWrite, io.ErrUnexpectedEOF):
		return trace.StatusCodeDataLoss
	default:
		return trace.StatusCodeUnknown
	}
}

// isAny returns true if errors.Is is true for any of the provided errors, errs.
func isAny(err error, errs ...error) bool {
	for _, e := range errs {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
