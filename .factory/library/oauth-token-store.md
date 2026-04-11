# OAuth Token Store

## Package: `internal/oauth/`

### Types

- **`TokenData`** — Struct representing a persisted OAuth token with fields: `AccessToken`, `TokenType`, `RefreshToken`, `Scope`, `ExpiresAt`, `RefreshIn`, `ObtainedAt`, `Source`
- **`TokenStore`** — Thread-safe token storage with JSON file persistence and in-memory caching

### Key Methods

- `NewTokenStore(filePath string) (*TokenStore, error)` — Creates store, creates parent dirs (0700), loads existing token from disk
- `Get() *TokenData` — Fast in-memory read (no disk I/O)
- `ValidToken() *TokenData` — Returns token only if not expired (with 30s clock skew margin)
- `Save(token *TokenData) error` — Saves to memory + disk (atomic write via temp+rename, 0600 permissions)
- `Clear() error` — Removes token from memory and deletes file
- `StartRefresh() (stillValid bool, done func(), err error)` — Refresh coordination: returns nil done if another refresh is in progress
- `SetRefreshError(err error)` — Logs refresh failure (can be extended for backoff tracking)

### TokenData Methods

- `IsExpired() bool` — Checks expiry with 30-second safety margin for clock skew
- `NeedsRefresh() bool` — Uses `RefreshIn` field or defaults to 80% of TTL

### Design Decisions

- **RWMutex** for reads/writes, separate **Mutex** for refresh coordination
- **Atomic writes**: write to temp file in same directory, then `os.Rename`
- **Stale token fallback**: during refresh, concurrent readers use cached (possibly near-expiry) token
- **Graceful corruption handling**: corrupted JSON files log a warning, store starts fresh
- **External file deletion**: in-memory token survives; file recreated on next save

### Usage Pattern for Token Refresh

```go
stillValid, done, err := ts.StartRefresh()
if err != nil { return err }
if done == nil {
    // Another refresh is in progress; use cached token
    return ts.ValidToken(), nil
}
defer done()
// ... perform actual token refresh ...
newToken := &TokenData{...}
if err := ts.Save(newToken); err != nil {
    ts.SetRefreshError(err)
    // Stale (but unexpired) token continues to be served
}
```
