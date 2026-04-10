# GitHub Token Discovery

## Package: `internal/oauth/`

### Types

- **`Discoverer`** — Discovers GitHub tokens from a priority chain of sources
- **`DiscovererOption`** — Functional options for configuring Discoverer

### Configuration Options

- `WithTokenStore(ts *TokenStore)` — Provide a TokenStore for persisted token fallback
- `WithGhCliPath(path string)` — Override path to `gh` CLI binary (default: `"gh"`)
- `WithHostsYmlPath(path string)` — Override path to hosts.yml (default: `~/.config/gh/hosts.yml`)

### Key Methods

- `NewDiscoverer(opts ...DiscovererOption) *Discoverer` — Creates a new Discoverer
- `DiscoverGitHubToken() (token string, source string, err error)` — Runs discovery chain

### Priority Chain

1. `COPILOT_GITHUB_TOKEN` env var → source: `"env:COPILOT_GITHUB_TOKEN"`
2. `GH_TOKEN` env var → source: `"env:GH_TOKEN"`
3. `GITHUB_TOKEN` env var → source: `"env:GITHUB_TOKEN"`
4. `gh auth token` CLI (5s timeout) → source: `"gh_cli"`
5. `~/.config/gh/hosts.yml` github.com entry → source: `"hosts.yml"`
6. Persisted token file (via TokenStore) → source: `"persisted"`

### Behavior

- Empty/whitespace-only values from any source are treated as missing (skipped)
- Returns `("", "", nil)` when no token is found (not an error)
- `gh auth token` runs in a separate process group with a 5-second timeout; on timeout, the entire process group is killed via SIGKILL
- Malformed `hosts.yml` files are silently skipped
- Missing `hosts.yml` files are silently skipped
- No TokenStore configured means persisted fallback is skipped

### Usage

```go
d := NewDiscoverer(WithTokenStore(ts))
token, source, err := d.DiscoverGitHubToken()
if err != nil { ... }
if token == "" { /* no token found */ }
fmt.Printf("found token from %s\n", source)
```

### Design Decisions

- **Process group kill**: `checkGhCli` starts `gh` in a new process group (`Setpgid: true`) and kills the entire group on timeout, preventing orphaned child processes
- **Stale persisted tokens**: Discovery returns even expired persisted tokens; the caller decides whether to use them or trigger a fresh exchange
- **Graceful degradation**: Every source failure is silently handled; discovery never errors, only returns empty
