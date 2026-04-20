package identity

import (
	"net/http"
	"runtime"
	"strings"
	"testing"
)

func TestGet_BuiltinProfiles(t *testing.T) {
	for _, id := range []string{"none", "codex-cli", "gemini-cli", "copilot-vscode", "opencode", "kimi-cli", "claude-code"} {
		p := Get(id)
		if p == nil {
			t.Errorf("Get(%q) returned nil", id)
			continue
		}
		if p.ID != id {
			t.Errorf("Get(%q).ID = %q", id, p.ID)
		}
		if p.DisplayName == "" {
			t.Errorf("Get(%q).DisplayName is empty", id)
		}
	}
}

func TestGet_Unknown(t *testing.T) {
	if p := Get("nonexistent"); p != nil {
		t.Errorf("Get(nonexistent) = %v, want nil", p)
	}
}

func TestList_ContainsAllBuiltins(t *testing.T) {
	profiles := List()
	ids := make(map[string]bool)
	for _, p := range profiles {
		ids[p.ID] = true
	}
	for _, want := range []string{"none", "codex-cli", "gemini-cli", "copilot-vscode", "opencode", "kimi-cli", "claude-code"} {
		if !ids[want] {
			t.Errorf("List() missing profile %q", want)
		}
	}
}

func TestIsBuiltin(t *testing.T) {
	if !IsBuiltin("codex-cli") {
		t.Error("IsBuiltin(codex-cli) = false")
	}
	if IsBuiltin("my-custom-profile") {
		t.Error("IsBuiltin(my-custom-profile) = true")
	}
}

func TestResolve_Empty_ReturnsNone(t *testing.T) {
	p := Resolve("", nil)
	if p == nil || p.ID != ProfileNoneID {
		t.Errorf("Resolve empty = %v, want none profile", p)
	}
}

func TestResolve_Builtin(t *testing.T) {
	p := Resolve("codex-cli", nil)
	if p == nil || p.ID != "codex-cli" {
		t.Errorf("Resolve(codex-cli) = %v", p)
	}
}

func TestResolve_CustomOverridesBuiltin(t *testing.T) {
	custom := []Profile{
		{ID: "codex-cli", DisplayName: "Custom Codex", UserAgent: "custom-ua"},
	}
	p := Resolve("codex-cli", custom)
	if p == nil || p.DisplayName != "Custom Codex" {
		t.Errorf("Resolve(codex-cli, custom) = %v, want custom profile", p)
	}
}

func TestResolve_CustomNew(t *testing.T) {
	custom := []Profile{
		{ID: "my-tool", DisplayName: "My Tool", UserAgent: "my-tool/1.0"},
	}
	p := Resolve("my-tool", custom)
	if p == nil || p.ID != "my-tool" {
		t.Errorf("Resolve(my-tool, custom) = %v", p)
	}
}

func TestResolve_UnknownFallsToNone(t *testing.T) {
	p := Resolve("does-not-exist", nil)
	if p == nil || p.ID != ProfileNoneID {
		t.Errorf("Resolve(does-not-exist) = %v, want none", p)
	}
}

func TestAllProfiles_IncludesCustom(t *testing.T) {
	custom := []Profile{
		{ID: "extra", DisplayName: "Extra"},
	}
	all := AllProfiles(custom)
	found := false
	for _, p := range all {
		if p.ID == "extra" {
			found = true
		}
	}
	if !found {
		t.Error("AllProfiles did not include custom profile")
	}
	if len(all) != len(List())+1 {
		t.Errorf("AllProfiles length = %d, want %d", len(all), len(List())+1)
	}
}

func TestRenderUserAgent_Static(t *testing.T) {
	p := Get("copilot-vscode")
	ua := p.RenderUserAgent(DefaultVars(""))
	if ua != "GitHubCopilot/1.0" {
		t.Errorf("copilot-vscode UA = %q", ua)
	}
}

func TestRenderUserAgent_TemplateVars(t *testing.T) {
	p := Get("codex-cli")
	ua := p.RenderUserAgent(DefaultVars(""))
	if !strings.Contains(ua, "codex_cli_rs/0.120.0") {
		t.Errorf("codex-cli UA missing version: %q", ua)
	}
	if !strings.Contains(ua, runtime.GOOS) {
		t.Errorf("codex-cli UA missing OS: %q", ua)
	}
}

func TestRenderUserAgent_ModelVar(t *testing.T) {
	p := Get("gemini-cli")
	ua := p.RenderUserAgent(DefaultVars("gemini-2.5-flash"))
	if !strings.Contains(ua, "gemini-2.5-flash") {
		t.Errorf("gemini-cli UA missing model: %q", ua)
	}
}

func TestRenderUserAgent_Empty(t *testing.T) {
	p := Get("none")
	ua := p.RenderUserAgent(DefaultVars(""))
	if ua != "" {
		t.Errorf("none profile UA = %q, want empty", ua)
	}
}

func TestApplyProfile_Nil(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	req.Header.Set("User-Agent", "original")
	ApplyProfile(req, nil, "")
	if req.Header.Get("User-Agent") != "original" {
		t.Error("nil profile modified User-Agent")
	}
}

func TestApplyProfile_None(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	req.Header.Set("User-Agent", "original")
	ApplyProfile(req, Get("none"), "")
	if req.Header.Get("User-Agent") != "original" {
		t.Error("none profile modified User-Agent")
	}
}

func TestApplyProfile_SetsHeaders(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	ApplyProfile(req, Get("codex-cli"), "gpt-4o")

	ua := req.Header.Get("User-Agent")
	if !strings.Contains(ua, "codex_cli_rs") {
		t.Errorf("User-Agent = %q, want codex_cli_rs", ua)
	}
	if req.Header.Get("originator") != "codex_cli_rs" {
		t.Errorf("originator header = %q", req.Header.Get("originator"))
	}
	if req.Header.Get("X-Request-Id") == "" {
		t.Error("X-Request-Id not set")
	}
}

func TestApplyProfile_CopilotHeaders(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	ApplyProfile(req, Get("copilot-vscode"), "")

	if req.Header.Get("User-Agent") != "GitHubCopilot/1.0" {
		t.Errorf("User-Agent = %q", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("Editor-Version") != "vscode/1.96.0" {
		t.Errorf("Editor-Version = %q", req.Header.Get("Editor-Version"))
	}
	if req.Header.Get("Editor-Plugin-Version") != "copilot-chat/0.24.0" {
		t.Errorf("Editor-Plugin-Version = %q", req.Header.Get("Editor-Plugin-Version"))
	}
	if req.Header.Get("Copilot-Integration-Id") != "vscode-chat" {
		t.Errorf("Copilot-Integration-Id = %q", req.Header.Get("Copilot-Integration-Id"))
	}
}

func TestApplyProfile_ClaudeSessionID(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	ApplyProfile(req, Get("claude-code"), "")

	sid := req.Header.Get("X-Claude-Code-Session-Id")
	if sid == "" {
		t.Error("X-Claude-Code-Session-Id not set")
	}
	req2, _ := http.NewRequest("POST", "https://example.com", nil)
	ApplyProfile(req2, Get("claude-code"), "")
	if req2.Header.Get("X-Claude-Code-Session-Id") != sid {
		t.Error("session ID changed between requests")
	}
}

func TestApplyProfile_ExistingRequestID_NotOverwritten(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	req.Header.Set("X-Request-Id", "existing-id")
	ApplyProfile(req, Get("codex-cli"), "")

	if req.Header.Get("X-Request-Id") != "existing-id" {
		t.Errorf("X-Request-Id was overwritten: %q", req.Header.Get("X-Request-Id"))
	}
}

func TestNormaliseArch(t *testing.T) {
	tests := []struct {
		goarch, want string
	}{
		{"amd64", "x64"},
		{"386", "x86"},
		{"arm64", "arm64"},
		{"unknown", "unknown"},
	}
	for _, tt := range tests {
		if got := normaliseArch(tt.goarch); got != tt.want {
			t.Errorf("normaliseArch(%q) = %q, want %q", tt.goarch, got, tt.want)
		}
	}
}
