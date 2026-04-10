package oauth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- Test helpers for token discovery ---

// helperSetenv sets an environment variable and returns a cleanup function.
func helperSetenv(t *testing.T, key, value string) func() {
	t.Helper()
	orig, ok := os.LookupEnv(key)
	os.Setenv(key, value)
	return func() {
		if ok {
			os.Setenv(key, orig)
		} else {
			os.Unsetenv(key)
		}
	}
}

// helperUnsetenv unsets an environment variable and returns a cleanup function.
func helperUnsetenv(t *testing.T, key string) func() {
	t.Helper()
	orig, ok := os.LookupEnv(key)
	os.Unsetenv(key)
	return func() {
		if ok {
			os.Setenv(key, orig)
		} else {
			os.Unsetenv(key)
		}
	}
}

// helperUnsetAllGitHubEnv clears all GitHub-related env vars and returns a cleanup.
func helperUnsetAllGitHubEnv(t *testing.T) func() {
	t.Helper()
	var cleanups []func()
	for _, key := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		cleanups = append(cleanups, helperUnsetenv(t, key))
	}
	return func() {
		for _, c := range cleanups {
			c()
		}
	}
}

// helperWriteHostsYML writes a gh hosts.yml file with the given content.
func helperWriteHostsYML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts.yml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writing hosts.yml: %v", err)
	}
	return path
}

// helperMakeIsolatedDiscovery creates a Discoverer with a temp token store
// that uses non-existent CLI and hosts.yml paths. Does NOT touch env vars.
// Caller should use helperUnsetAllGitHubEnv before calling this if needed.
func helperMakeIsolatedDiscovery(t *testing.T) (*Discoverer, func()) {
	t.Helper()
	dir := t.TempDir()
	ts, err := NewTokenStore(filepath.Join(dir, "test-token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}
	d := NewDiscoverer(
		WithTokenStore(ts),
		WithGhCliPath(filepath.Join(dir, "nonexistent-gh")),
		WithHostsYmlPath(filepath.Join(dir, "nonexistent-hosts.yml")),
	)
	return d, func() {}
}

// helperDiscovererWithMockGh creates a Discoverer with a mock gh CLI script
// that prints the given output. Does NOT touch env vars.
func helperDiscovererWithMockGh(t *testing.T, ghOutput string) (*Discoverer, func()) {
	t.Helper()
	dir := t.TempDir()
	mockGh := filepath.Join(dir, "gh")
	script := "#!/bin/sh\necho \"" + ghOutput + "\""
	if err := os.WriteFile(mockGh, []byte(script), 0755); err != nil {
		t.Fatalf("writing mock gh: %v", err)
	}
	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}
	d := NewDiscoverer(
		WithTokenStore(ts),
		WithGhCliPath(mockGh),
		WithHostsYmlPath(filepath.Join(dir, "nonexistent-hosts.yml")),
	)
	return d, func() {}
}

// --- VAL-TOKEN-001: COPILOT_GITHUB_TOKEN env var is checked first ---

func TestDiscoverGitHubToken_COPILOT_GITHUB_TOKEN(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	c1 := helperSetenv(t, "COPILOT_GITHUB_TOKEN", "ghp_copilot_test_token_123")
	defer c1()

	d, _ := helperMakeIsolatedDiscovery(t)
	token, source, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "ghp_copilot_test_token_123" {
		t.Errorf("token = %q, want %q", token, "ghp_copilot_test_token_123")
	}
	if source != "env:COPILOT_GITHUB_TOKEN" {
		t.Errorf("source = %q, want %q", source, "env:COPILOT_GITHUB_TOKEN")
	}
}

// --- VAL-TOKEN-002: GH_TOKEN env var is checked second ---

func TestDiscoverGitHubToken_GH_TOKEN(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	c1 := helperSetenv(t, "GH_TOKEN", "ghp_gh_test_token_456")
	defer c1()

	d, _ := helperMakeIsolatedDiscovery(t)
	token, source, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "ghp_gh_test_token_456" {
		t.Errorf("token = %q, want %q", token, "ghp_gh_test_token_456")
	}
	if source != "env:GH_TOKEN" {
		t.Errorf("source = %q, want %q", source, "env:GH_TOKEN")
	}
}

// --- VAL-TOKEN-003: GITHUB_TOKEN env var is checked third ---

func TestDiscoverGitHubToken_GITHUB_TOKEN(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	c1 := helperSetenv(t, "GITHUB_TOKEN", "ghp_github_test_token_789")
	defer c1()

	d, _ := helperMakeIsolatedDiscovery(t)
	token, source, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "ghp_github_test_token_789" {
		t.Errorf("token = %q, want %q", token, "ghp_github_test_token_789")
	}
	if source != "env:GITHUB_TOKEN" {
		t.Errorf("source = %q, want %q", source, "env:GITHUB_TOKEN")
	}
}

// --- VAL-TOKEN-004: gh auth token CLI fallback ---

func TestDiscoverGitHubToken_GhCli(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	d, _ := helperDiscovererWithMockGh(t, "ghp_cli_mock_token")
	token, source, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "ghp_cli_mock_token" {
		t.Errorf("token = %q, want %q", token, "ghp_cli_mock_token")
	}
	if source != "gh_cli" {
		t.Errorf("source = %q, want %q", source, "gh_cli")
	}
}

// --- gh CLI invocation has timeout protection ---

func TestDiscoverGitHubToken_GhCliTimeout(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	dir := t.TempDir()
	// Create a mock gh CLI that sleeps for a long time
	mockGh := filepath.Join(dir, "gh")
	script := "#!/bin/sh\nsleep 30"
	if err := os.WriteFile(mockGh, []byte(script), 0755); err != nil {
		t.Fatalf("writing mock gh: %v", err)
	}

	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}

	d := NewDiscoverer(
		WithTokenStore(ts),
		WithGhCliPath(mockGh),
		WithHostsYmlPath(filepath.Join(dir, "nonexistent-hosts.yml")),
	)

	start := time.Now()
	token, _, err := d.DiscoverGitHubToken()
	elapsed := time.Since(start)

	// Should complete well under 10 seconds due to the 5s timeout
	if elapsed > 10*time.Second {
		t.Errorf("DiscoverGitHubToken took %v, should have timed out within ~5s", elapsed)
	}
	// Token should be empty (CLI timed out, fell through to other sources)
	if token != "" {
		t.Errorf("expected empty token on CLI timeout, got %q", token)
	}
}

// --- gh CLI with non-zero exit code is skipped ---

func TestDiscoverGitHubToken_GhCliFailure(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	dir := t.TempDir()
	mockGh := filepath.Join(dir, "gh")
	script := "#!/bin/sh\necho 'error: not authenticated' >&2\nexit 1"
	if err := os.WriteFile(mockGh, []byte(script), 0755); err != nil {
		t.Fatalf("writing mock gh: %v", err)
	}

	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}

	d := NewDiscoverer(
		WithTokenStore(ts),
		WithGhCliPath(mockGh),
		WithHostsYmlPath(filepath.Join(dir, "nonexistent-hosts.yml")),
	)

	token, _, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "" {
		t.Errorf("expected empty token on CLI failure, got %q", token)
	}
}

// --- VAL-TOKEN-005: hosts.yml github.com entry is parsed ---

func TestDiscoverGitHubToken_HostsYml(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	dir := t.TempDir()
	hostsPath := helperWriteHostsYML(t, `github.com:
    user: Aeversil
    oauth_token: ghp_hosts_yml_token
    git_protocol: ssh
`)

	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}

	d := NewDiscoverer(
		WithTokenStore(ts),
		WithGhCliPath(filepath.Join(dir, "nonexistent-gh")),
		WithHostsYmlPath(hostsPath),
	)

	token, source, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "ghp_hosts_yml_token" {
		t.Errorf("token = %q, want %q", token, "ghp_hosts_yml_token")
	}
	if source != "hosts.yml" {
		t.Errorf("source = %q, want %q", source, "hosts.yml")
	}
}

// --- hosts.yml parsing handles malformed YAML gracefully ---

func TestDiscoverGitHubToken_HostsYmlMalformed(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	dir := t.TempDir()
	hostsPath := helperWriteHostsYML(t, `{{{{invalid yaml:::`)

	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}

	d := NewDiscoverer(
		WithTokenStore(ts),
		WithGhCliPath(filepath.Join(dir, "nonexistent-gh")),
		WithHostsYmlPath(hostsPath),
	)

	token, _, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() should not error on malformed YAML: %v", err)
	}
	if token != "" {
		t.Errorf("expected empty token on malformed YAML, got %q", token)
	}
}

// --- hosts.yml with no github.com entry ---

func TestDiscoverGitHubToken_HostsYmlNoGithub(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	dir := t.TempDir()
	hostsPath := helperWriteHostsYML(t, `gitlab.com:
    user: test
    oauth_token: glpat-xyz
`)

	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}

	d := NewDiscoverer(
		WithTokenStore(ts),
		WithGhCliPath(filepath.Join(dir, "nonexistent-gh")),
		WithHostsYmlPath(hostsPath),
	)

	token, _, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "" {
		t.Errorf("expected empty token when github.com not in hosts.yml, got %q", token)
	}
}

// --- hosts.yml with empty oauth_token ---

func TestDiscoverGitHubToken_HostsYmlEmptyToken(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	dir := t.TempDir()
	hostsPath := helperWriteHostsYML(t, `github.com:
    user: Aeversil
    oauth_token: ""
    git_protocol: ssh
`)

	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}

	d := NewDiscoverer(
		WithTokenStore(ts),
		WithGhCliPath(filepath.Join(dir, "nonexistent-gh")),
		WithHostsYmlPath(hostsPath),
	)

	token, _, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "" {
		t.Errorf("expected empty token when oauth_token is empty, got %q", token)
	}
}

// --- VAL-TOKEN-006: Persisted token file is checked last ---

func TestDiscoverGitHubToken_PersistedToken(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	dir := t.TempDir()
	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}

	// Save a token to the store
	savedToken := &TokenData{
		AccessToken: "ghp_persisted_token_abc",
		TokenType:   "Bearer",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		ObtainedAt:  time.Now(),
		Source:      "env:GH_TOKEN",
	}
	if err := ts.Save(savedToken); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	d := NewDiscoverer(
		WithTokenStore(ts),
		WithGhCliPath(filepath.Join(dir, "nonexistent-gh")),
		WithHostsYmlPath(filepath.Join(dir, "nonexistent-hosts.yml")),
	)

	token, source, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "ghp_persisted_token_abc" {
		t.Errorf("token = %q, want %q", token, "ghp_persisted_token_abc")
	}
	if source != "persisted" {
		t.Errorf("source = %q, want %q", source, "persisted")
	}
}

// --- Persisted token that is expired should still be discovered (caller decides) ---

func TestDiscoverGitHubToken_ExpiredPersistedToken(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	dir := t.TempDir()
	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}

	// Save an expired token
	expiredToken := &TokenData{
		AccessToken: "ghp_expired_persisted",
		TokenType:   "Bearer",
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
		ObtainedAt:  time.Now().Add(-2 * time.Hour),
		Source:      "env:GH_TOKEN",
	}
	if err := ts.Save(expiredToken); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	d := NewDiscoverer(
		WithTokenStore(ts),
		WithGhCliPath(filepath.Join(dir, "nonexistent-gh")),
		WithHostsYmlPath(filepath.Join(dir, "nonexistent-hosts.yml")),
	)

	token, _, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	// Discovery returns the expired token; the caller (Copilot token exchange) decides
	// whether to use it or request a fresh one.
	if token != "ghp_expired_persisted" {
		t.Errorf("expected expired persisted token, got %q", token)
	}
}

// --- VAL-TOKEN-007: Priority chain order ---

func TestDiscoverGitHubToken_PriorityOrder(t *testing.T) {
	// When all sources are available, COPILOT_GITHUB_TOKEN wins
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	c1 := helperSetenv(t, "COPILOT_GITHUB_TOKEN", "ghp_priority_1")
	defer c1()
	c2 := helperSetenv(t, "GH_TOKEN", "ghp_priority_2")
	defer c2()
	c3 := helperSetenv(t, "GITHUB_TOKEN", "ghp_priority_3")
	defer c3()

	dir := t.TempDir()
	hostsPath := helperWriteHostsYML(t, `github.com:
    user: Aeversil
    oauth_token: ghp_priority_5
`)
	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}
	savedToken := &TokenData{
		AccessToken: "ghp_priority_6",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		ObtainedAt:  time.Now(),
	}
	if err := ts.Save(savedToken); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	d, _ := helperDiscovererWithMockGh(t, "ghp_priority_4")
	// Override the token store and hosts.yml
	d.tokenStore = ts
	d.hostsYmlPath = hostsPath

	token, source, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "ghp_priority_1" {
		t.Errorf("token = %q, want %q (highest priority)", token, "ghp_priority_1")
	}
	if source != "env:COPILOT_GITHUB_TOKEN" {
		t.Errorf("source = %q, want %q", source, "env:COPILOT_GITHUB_TOKEN")
	}
}

func TestDiscoverGitHubToken_PriorityOrder_GH_TOKEN(t *testing.T) {
	// When COPILOT_GITHUB_TOKEN is unset, GH_TOKEN wins
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	c1 := helperSetenv(t, "GH_TOKEN", "ghp_priority_2")
	defer c1()
	c2 := helperSetenv(t, "GITHUB_TOKEN", "ghp_priority_3")
	defer c2()

	d, _ := helperMakeIsolatedDiscovery(t)
	token, source, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "ghp_priority_2" {
		t.Errorf("token = %q, want %q", token, "ghp_priority_2")
	}
	if source != "env:GH_TOKEN" {
		t.Errorf("source = %q, want %q", source, "env:GH_TOKEN")
	}
}

func TestDiscoverGitHubToken_PriorityOrder_GITHUB_TOKEN(t *testing.T) {
	// When only GITHUB_TOKEN is set
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	c1 := helperSetenv(t, "GITHUB_TOKEN", "ghp_priority_3")
	defer c1()

	d, _ := helperMakeIsolatedDiscovery(t)
	token, source, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "ghp_priority_3" {
		t.Errorf("token = %q, want %q", token, "ghp_priority_3")
	}
	if source != "env:GITHUB_TOKEN" {
		t.Errorf("source = %q, want %q", source, "env:GITHUB_TOKEN")
	}
}

func TestDiscoverGitHubToken_PriorityOrder_CliOverHostsYml(t *testing.T) {
	// gh CLI beats hosts.yml
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	dir := t.TempDir()
	hostsPath := helperWriteHostsYML(t, `github.com:
    user: Aeversil
    oauth_token: ghp_hosts_lower
`)
	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}

	d, _ := helperDiscovererWithMockGh(t, "ghp_cli_higher")
	d.tokenStore = ts
	d.hostsYmlPath = hostsPath

	token, source, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "ghp_cli_higher" {
		t.Errorf("token = %q, want %q (CLI > hosts.yml)", token, "ghp_cli_higher")
	}
	if source != "gh_cli" {
		t.Errorf("source = %q, want %q", source, "gh_cli")
	}
}

func TestDiscoverGitHubToken_PriorityOrder_HostsYmlOverPersisted(t *testing.T) {
	// hosts.yml beats persisted token
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	dir := t.TempDir()
	hostsPath := helperWriteHostsYML(t, `github.com:
    user: Aeversil
    oauth_token: ghp_hosts_higher
`)
	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}
	savedToken := &TokenData{
		AccessToken: "ghp_persisted_lower",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		ObtainedAt:  time.Now(),
	}
	if err := ts.Save(savedToken); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	d := NewDiscoverer(
		WithTokenStore(ts),
		WithGhCliPath(filepath.Join(dir, "nonexistent-gh")),
		WithHostsYmlPath(hostsPath),
	)

	token, source, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "ghp_hosts_higher" {
		t.Errorf("token = %q, want %q (hosts.yml > persisted)", token, "ghp_hosts_higher")
	}
	if source != "hosts.yml" {
		t.Errorf("source = %q, want %q", source, "hosts.yml")
	}
}

// --- VAL-TOKEN-008: No sources available returns empty string with no error ---

func TestDiscoverGitHubToken_NoSources(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	d, _ := helperMakeIsolatedDiscovery(t)

	token, source, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() should not error when no sources: %v", err)
	}
	if token != "" {
		t.Errorf("expected empty token, got %q", token)
	}
	if source != "" {
		t.Errorf("expected empty source, got %q", source)
	}
}

// --- Empty/whitespace-only env vars are treated as missing ---

func TestDiscoverGitHubToken_EmptyEnvVarSkipped(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	// Set all env vars to empty/whitespace
	c1 := helperSetenv(t, "COPILOT_GITHUB_TOKEN", "  ")
	defer c1()
	c2 := helperSetenv(t, "GH_TOKEN", "")
	defer c2()
	c3 := helperSetenv(t, "GITHUB_TOKEN", "\t\n")
	defer c3()

	dir := t.TempDir()
	hostsPath := helperWriteHostsYML(t, `github.com:
    user: Aeversil
    oauth_token: ghp_hosts_fallback
`)

	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}

	d := NewDiscoverer(
		WithTokenStore(ts),
		WithGhCliPath(filepath.Join(dir, "nonexistent-gh")),
		WithHostsYmlPath(hostsPath),
	)

	token, source, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	// Should fall through to hosts.yml since env vars are whitespace-only
	if token != "ghp_hosts_fallback" {
		t.Errorf("expected hosts.yml fallback since env vars are empty, got %q", token)
	}
	if source != "hosts.yml" {
		t.Errorf("source = %q, want %q", source, "hosts.yml")
	}
}

// --- hosts.yml with whitespace-only oauth_token is skipped ---

func TestDiscoverGitHubToken_HostsYmlWhitespaceToken(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	dir := t.TempDir()
	hostsPath := helperWriteHostsYML(t, `github.com:
    user: Aeversil
    oauth_token: "   "
`)

	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}

	d := NewDiscoverer(
		WithTokenStore(ts),
		WithGhCliPath(filepath.Join(dir, "nonexistent-gh")),
		WithHostsYmlPath(hostsPath),
	)

	token, _, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "" {
		t.Errorf("expected empty token with whitespace-only hosts.yml token, got %q", token)
	}
}

// --- gh CLI returns whitespace-only output is skipped ---

func TestDiscoverGitHubToken_GhCliWhitespaceOutput(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	d, _ := helperDiscovererWithMockGh(t, "   ")

	token, _, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "" {
		t.Errorf("expected empty token with whitespace-only CLI output, got %q", token)
	}
}

// --- Missing hosts.yml file is handled gracefully ---

func TestDiscoverGitHubToken_MissingHostsYml(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	d, _ := helperMakeIsolatedDiscovery(t)

	token, _, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "" {
		t.Errorf("expected empty token with missing hosts.yml, got %q", token)
	}
}

// --- Persisted token with empty access token is skipped ---

func TestDiscoverGitHubToken_PersistedEmptyToken(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	dir := t.TempDir()
	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}

	// Save a token with empty access token
	savedToken := &TokenData{
		AccessToken: "",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		ObtainedAt:  time.Now(),
	}
	if err := ts.Save(savedToken); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	d := NewDiscoverer(
		WithTokenStore(ts),
		WithGhCliPath(filepath.Join(dir, "nonexistent-gh")),
		WithHostsYmlPath(filepath.Join(dir, "nonexistent-hosts.yml")),
	)

	token, _, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "" {
		t.Errorf("expected empty token with empty persisted access token, got %q", token)
	}
}

// --- No TokenStore configured still works (skips persisted check) ---

func TestDiscoverGitHubToken_NoTokenStore(t *testing.T) {
	cleanup := helperUnsetAllGitHubEnv(t)
	defer cleanup()

	dir := t.TempDir()
	d := NewDiscoverer(
		WithGhCliPath(filepath.Join(dir, "nonexistent-gh")),
		WithHostsYmlPath(filepath.Join(dir, "nonexistent-hosts.yml")),
	)

	token, _, err := d.DiscoverGitHubToken()
	if err != nil {
		t.Fatalf("DiscoverGitHubToken() error: %v", err)
	}
	if token != "" {
		t.Errorf("expected empty token with no sources, got %q", token)
	}
}
