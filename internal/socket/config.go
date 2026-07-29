package socket

import (
	"context"
	"errors"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bojanrajkovic/runny/internal/home"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// GetConfig returns the daemon's config.yaml verbatim.
//
// On a system home the operator has no read access to the file -- config.yaml
// is 0600 and daemon-owned, and darwin's operator entry stops at the home
// directory -- so this is how an operator reads its own config: `edit-config`
// seeds an editor from it, `upgrade-daemon` validates these bytes against the
// binary it is upgrading to. Bytes, not a parsed
// document: a human re-edits what this returns, so comments, key order,
// quoting and the schema modeline all have to survive.
//
// A missing config is NotFound and nothing else is. edit-config's fallback for
// that answer is a blank skeleton, which would destroy a working config if a
// read the daemon merely FAILED could reach it — so the codes stay disjoint
// and every other failure is Internal.
func (s *Server) GetConfig(_ context.Context, _ *runnyv1.GetConfigRequest) (*runnyv1.GetConfigResponse, error) {
	path := s.HomeDir.ConfigPath()
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, status.Errorf(codes.NotFound, "no config at %s", path)
	case err != nil:
		return nil, status.Errorf(codes.Internal, "reading %s: %v", path, err)
	}
	return &runnyv1.GetConfigResponse{Content: b}, nil
}

// SetConfig replaces config.yaml with req.Content, atomically.
//
// The daemon writing it is the point, not an implementation detail: an
// operator cannot chown a file to the service account (only root can), so a
// rename from the operator's own temp file would leave the daemon's config
// owned by whichever operator edited last. Routing the write through the owner
// is the only way ownership stays put.
//
// It does not validate, and does not reload. See the RPC's contract comment in
// runny.proto for why a parser gate here would re-break upgrade-daemon's
// forward-only edit.
func (s *Server) SetConfig(_ context.Context, req *runnyv1.SetConfigRequest) (*runnyv1.SetConfigResponse, error) {
	if err := home.AtomicWrite(s.HomeDir.ConfigPath(), req.Content, 0o600); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &runnyv1.SetConfigResponse{}, nil
}
