# GitHub Token Discovery (DEPRECATED)

> **DEPRECATED**: This module is no longer used by CopilotBackend as of the oauth-refactor milestone.
> CopilotBackend now uses the GitHub Device Code Flow (`internal/oauth/device_code.go`).
>
> The files `internal/oauth/token_discovery.go` and `token_discovery_test.go` still exist
> but are dead code. They may be removed in a future cleanup feature.
>
> See `copilot-backend.md` for the current Copilot authentication approach.

## Original Purpose

The `Discoverer` type in `internal/oauth/token_discovery.go` discovered GitHub tokens from a priority chain of sources: `COPILOT_GITHUB_TOKEN` → `GH_TOKEN` → `GITHUB_TOKEN` → `gh auth token` CLI → `~/.config/gh/hosts.yml` → persisted token file.

This was replaced by the Device Code Flow in commit `800db77` (feature `copilot-device-code-flow`).
