package oauth

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// ghCliTimeout is the maximum time to wait for `gh auth token` to complete.
	ghCliTimeout = 5 * time.Second
)

// Discoverer finds a GitHub token from multiple sources in priority order:
//  1. COPILOT_GITHUB_TOKEN environment variable
//  2. GH_TOKEN environment variable
//  3. GITHUB_TOKEN environment variable
//  4. gh auth token CLI (with timeout)
//  5. ~/.config/gh/hosts.yml (github.com entry)
//  6. Persisted token file (via TokenStore)
//
// Empty or whitespace-only values are treated as missing.
// DiscoverGitHubToken returns ("", "", nil) when no token is found.
type Discoverer struct {
	tokenStore   *TokenStore
	ghCliPath    string
	hostsYmlPath string
}

// DiscovererOption configures a Discoverer.
type DiscovererOption func(*Discoverer)

// WithTokenStore sets the TokenStore used for persisted token discovery.
func WithTokenStore(ts *TokenStore) DiscovererOption {
	return func(d *Discoverer) {
		d.tokenStore = ts
	}
}

// WithGhCliPath sets the path to the gh CLI binary. Defaults to "gh".
func WithGhCliPath(path string) DiscovererOption {
	return func(d *Discoverer) {
		d.ghCliPath = path
	}
}

// WithHostsYmlPath sets the path to the gh hosts.yml file.
// Defaults to ~/.config/gh/hosts.yml.
func WithHostsYmlPath(path string) DiscovererOption {
	return func(d *Discoverer) {
		d.hostsYmlPath = path
	}
}

// NewDiscoverer creates a new Discoverer with the given options.
func NewDiscoverer(opts ...DiscovererOption) *Discoverer {
	d := &Discoverer{
		ghCliPath: "gh",
	}
	for _, opt := range opts {
		opt(d)
	}
	if d.hostsYmlPath == "" {
		homeDir, err := os.UserHomeDir()
		if err == nil {
			d.hostsYmlPath = filepath.Join(homeDir, ".config", "gh", "hosts.yml")
		}
	}
	return d
}

// DiscoverGitHubToken attempts to find a GitHub token from the priority chain
// of sources. Returns the token value, a source label (e.g., "env:GH_TOKEN"),
// and an error. When no token is found, returns ("", "", nil) — not an error.
func (d *Discoverer) DiscoverGitHubToken() (token string, source string, err error) {
	// 1. Check COPILOT_GITHUB_TOKEN
	if t := checkEnvVar("COPILOT_GITHUB_TOKEN"); t != "" {
		return t, "env:COPILOT_GITHUB_TOKEN", nil
	}

	// 2. Check GH_TOKEN
	if t := checkEnvVar("GH_TOKEN"); t != "" {
		return t, "env:GH_TOKEN", nil
	}

	// 3. Check GITHUB_TOKEN
	if t := checkEnvVar("GITHUB_TOKEN"); t != "" {
		return t, "env:GITHUB_TOKEN", nil
	}

	// 4. Check gh auth token CLI
	if t, err := d.checkGhCli(); err != nil {
		// Log but don't fail — continue to next source
		_ = err
	} else if t != "" {
		return t, "gh_cli", nil
	}

	// 5. Check ~/.config/gh/hosts.yml
	if t := d.checkHostsYml(); t != "" {
		return t, "hosts.yml", nil
	}

	// 6. Check persisted token file
	if t := d.checkPersistedToken(); t != "" {
		return t, "persisted", nil
	}

	return "", "", nil
}

// checkEnvVar returns the trimmed value of the environment variable, or empty
// string if the variable is unset, empty, or whitespace-only.
func checkEnvVar(key string) string {
	val := strings.TrimSpace(os.Getenv(key))
	return val
}

// checkGhCli runs `gh auth token` with a timeout and returns the trimmed output.
// Returns empty string if the CLI is not available, exits non-zero, or times out.
// The command is started in its own process group so the timeout can kill the
// entire process tree reliably.
func (d *Discoverer) checkGhCli() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ghCliTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.ghCliPath, "auth", "token")
	// Start in a new process group so timeout kills the whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("gh auth token: %w", err)
	}

	// Wait in a goroutine so we can enforce the timeout.
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	select {
	case err := <-waitDone:
		if err != nil {
			return "", fmt.Errorf("gh auth token: %w", err)
		}
		token := strings.TrimSpace(stdout.String())
		return token, nil
	case <-ctx.Done():
		// Kill the entire process group.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waitDone // Wait for cleanup to complete
		return "", fmt.Errorf("gh auth token: timed out after %v", ghCliTimeout)
	}
}

// hostsYml represents the structure of ~/.config/gh/hosts.yml.
type hostsYml map[string]struct {
	OAuthToken string `yaml:"oauth_token"`
}

// checkHostsYml reads the hosts.yml file and extracts the oauth_token for
// github.com. Returns empty string if the file doesn't exist, is malformed,
// or has no github.com entry with a non-empty oauth_token.
func (d *Discoverer) checkHostsYml() string {
	if d.hostsYmlPath == "" {
		return ""
	}

	data, err := os.ReadFile(d.hostsYmlPath)
	if err != nil {
		return ""
	}

	var hosts hostsYml
	if err := yaml.Unmarshal(data, &hosts); err != nil {
		// Malformed YAML — skip silently
		return ""
	}

	entry, ok := hosts["github.com"]
	if !ok {
		return ""
	}

	token := strings.TrimSpace(entry.OAuthToken)
	return token
}

// checkPersistedToken reads the token from the TokenStore if available.
// Returns the access token if it's non-empty.
func (d *Discoverer) checkPersistedToken() string {
	if d.tokenStore == nil {
		return ""
	}

	token := d.tokenStore.Get()
	if token == nil {
		return ""
	}

	access := strings.TrimSpace(token.AccessToken)
	return access
}
