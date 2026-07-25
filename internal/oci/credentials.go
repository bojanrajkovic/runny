package oci

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// dockerConfig is the subset of docker/oras/skopeo's config.json this package
// reads. Only Auths carries a usable credential; CredHelpers and CredsStore
// are parsed solely to DIAGNOSE a config that delegates to a helper (see
// credentialsFor) -- runny never runs one.
type dockerConfig struct {
	Auths       map[string]struct{ Auth string } `json:"auths"`
	CredHelpers map[string]string                `json:"credHelpers"`
	CredsStore  string                           `json:"credsStore"`
}

// CredentialConfigPath is the credential file this package reads, resolved the
// same way credentialsFor resolves it (empty if it cannot be determined).
// Exported so a caller can NAME the file in a diagnostic -- a 401 on a private
// image is nearly always a missing or mis-keyed entry in this exact path, and
// the operator cannot check it without being told which one it is.
func CredentialConfigPath() string {
	path, err := dockerConfigPath()
	if err != nil {
		return ""
	}
	return path
}

// dockerConfigPath is $DOCKER_CONFIG/config.json, defaulting to
// ~/.docker/config.json -- the same default docker/oras/skopeo use.
func dockerConfigPath() (string, error) {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".docker", "config.json"), nil
}

// credentialsFor resolves (username, password) for host from a static
// auths[host].auth entry in the docker/oras/skopeo config file.
//
// Credential HELPERS are deliberately not supported. The deployment that
// needs private pulls is the system daemon, which runs as a service account
// that cannot reach the operator's login keychain at all -- so on macOS,
// where `docker login` defaults to credsStore: "osxkeychain", a helper could
// never produce a credential for it no matter where the config file lives.
// Supporting helpers therefore only ever served an operator at a terminal,
// while dragging in a subprocess, a PATH that launchd does not supply, and
// docker's helper-vs-inline precedence rules. A static entry is what the
// daemon needs regardless; that is the whole feature.
//
// ok=false means "pull anonymously", exactly as this package always has. The
// quiet cases are the normal ones -- no config file, no entry for this host.
// A config that is present but unusable, or one that delegates THIS host to a
// helper, warns instead: the alternative is a bare 401 with no trace of why.
func credentialsFor(host string) (username, password string, ok bool) {
	path, err := dockerConfigPath()
	if err != nil {
		return "", "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		// Absent is the common case -- every public pull on a host that never
		// logged in anywhere -- and stays quiet. Anything else means the file
		// is THERE and we still couldn't read it, which for the system daemon
		// is usually the home's inheriting ACL not granting the service
		// account read: the operator sees a file they can read perfectly well
		// themselves, and without this the only symptom is a bare 401.
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("registry credential config is unreadable; falling back to an anonymous pull",
				"path", path, "err", err)
		}
		return "", "", false
	}
	var cfg dockerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		slog.Warn("registry credential config is not valid JSON; falling back to an anonymous pull",
			"path", path, "err", err)
		return "", "", false
	}
	entry, hasEntry := cfg.Auths[host]
	if hasEntry {
		if user, pass, ok := decodeAuth(entry.Auth); ok {
			return user, pass, true
		}
		// Present but undecodable is a typo, not a delegation -- the docs have
		// the operator base64 the credential by hand, so a truncated or
		// mangled string is a likely mistake and must not look identical to
		// having configured nothing. An EMPTY auth is different: that is the
		// stub a helper-backed login leaves, diagnosed below.
		if entry.Auth != "" {
			slog.Warn("registry credential entry is not decodable as base64 user:pass; falling back to an anonymous pull",
				"host", host, "path", path)
			return "", "", false
		}
	}
	// A keychain-backed `docker login` leaves an auths entry for the host with
	// no "auth" field -- the secret went to the store. Say so, because the
	// operator's config LOOKS like it holds this credential. Scoped to hosts
	// the operator actually addressed (a stub entry, or an explicit
	// per-host helper): a bare global credsStore is set on most macOS
	// machines, and warning on it would fire for every public image pulled.
	if helper := cfg.CredHelpers[host]; helper != "" || (hasEntry && cfg.CredsStore != "") {
		slog.Warn("registry credentials for this host are held by a credential helper, which runny does not use; write a static auths entry instead (see docs/deploy.md)",
			"host", host, "path", path)
	}
	return "", "", false
}

func decodeAuth(auth string) (username, password string, ok bool) {
	dec, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		return "", "", false
	}
	user, pass, found := strings.Cut(string(dec), ":")
	if !found {
		return "", "", false
	}
	return user, pass, true
}
