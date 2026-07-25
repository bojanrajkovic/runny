package oci

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// dockerConfig is the subset of docker/oras/skopeo's config.json this
// package reads. Static credentials live in Auths; CredHelpers (falling
// back to CredsStore) name a docker-credential-<name> helper binary on
// PATH.
type dockerConfig struct {
	Auths       map[string]struct{ Auth string } `json:"auths"`
	CredHelpers map[string]string                `json:"credHelpers"`
	CredsStore  string                           `json:"credsStore"`
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

// credentialsFor resolves (username, password) for host from the
// docker/oras/skopeo config file: a static auths[host].auth entry, or a
// credHelpers[host] (falling back to credsStore) helper binary. ok=false
// covers every "no creds configured for this host" case -- no config file,
// unparseable config, no matching entry, no helper binary, or a helper that
// exited non-zero -- so a caller never has to distinguish "not configured"
// from "misconfigured"; both just mean the pull proceeds anonymous, as it
// always has.
func credentialsFor(ctx context.Context, host string) (username, password string, ok bool) {
	path, err := dockerConfigPath()
	if err != nil {
		return "", "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	var cfg dockerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", "", false
	}
	if a, ok := cfg.Auths[host]; ok {
		if user, pass, ok := decodeAuth(a.Auth); ok {
			return user, pass, true
		}
	}
	helper := cfg.CredHelpers[host]
	if helper == "" {
		helper = cfg.CredsStore
	}
	if helper == "" {
		return "", "", false
	}
	return runCredentialHelper(ctx, helper, host)
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

// runCredentialHelper speaks the standard docker-credential-<name> "get"
// protocol: the host on stdin, {"Username","Secret"} JSON on stdout. A
// missing binary or non-zero exit just means this helper has no creds for
// host, not an error.
func runCredentialHelper(ctx context.Context, name, host string) (username, password string, ok bool) {
	cmd := exec.CommandContext(ctx, "docker-credential-"+name, "get")
	cmd.Stdin = strings.NewReader(host)
	out, err := cmd.Output()
	if err != nil {
		return "", "", false
	}
	var resp struct{ Username, Secret string }
	if err := json.Unmarshal(out, &resp); err != nil || resp.Username == "" || resp.Secret == "" {
		return "", "", false
	}
	return resp.Username, resp.Secret, true
}
