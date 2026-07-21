package oc

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"

	"go.opencensus.io/trace"
)

func TestToStatusCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"invalid argument", os.ErrInvalid, trace.StatusCodeInvalidArgument},
		{"deadline exceeded", os.ErrDeadlineExceeded, trace.StatusCodeDeadlineExceeded},
		{"not exist", os.ErrNotExist, trace.StatusCodeNotFound},
		{"exist", os.ErrExist, trace.StatusCodeAlreadyExists},
		{"permission", os.ErrPermission, trace.StatusCodePermissionDenied},
		{"closed", os.ErrClosed, trace.StatusCodeFailedPrecondition},
		{"net closed", net.ErrClosed, trace.StatusCodeFailedPrecondition},
		{"closed pipe", io.ErrClosedPipe, trace.StatusCodeFailedPrecondition},
		{"short buffer", io.ErrShortBuffer, trace.StatusCodeFailedPrecondition},
		{"no progress", io.ErrNoProgress, trace.StatusCodeInternal},
		{"short write", io.ErrShortWrite, trace.StatusCodeDataLoss},
		{"unexpected EOF", io.ErrUnexpectedEOF, trace.StatusCodeDataLoss},
		{"wrapped", fmt.Errorf("open failed: %w", os.ErrNotExist), trace.StatusCodeNotFound},
		{"unmatched", errors.New("something else entirely"), trace.StatusCodeUnknown},
		{"nil", nil, trace.StatusCodeUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toStatusCode(tt.err); got != tt.want {
				t.Errorf("toStatusCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsAny(t *testing.T) {
	if isAny(os.ErrNotExist) {
		t.Error("isAny with no candidates should be false")
	}
	if !isAny(fmt.Errorf("wrap: %w", os.ErrNotExist), os.ErrPermission, os.ErrNotExist) {
		t.Error("isAny should match a wrapped error among the candidates")
	}
	if isAny(os.ErrNotExist, os.ErrPermission, os.ErrExist) {
		t.Error("isAny should not match when none of the candidates apply")
	}
}
