package identity

import (
	"bytes"
	"runtime"
	"sync"
	"text/template"

	"github.com/google/uuid"
)

// Profile defines an identity profile — a named bundle of User-Agent string
// and HTTP headers that make the proxy's outgoing requests resemble a specific
// CLI tool.
type Profile struct {
	ID          string            // unique identifier, e.g. "codex-cli"
	DisplayName string            // human-readable label for the UI
	UserAgent   string            // Go text/template; empty = don't override
	Headers     map[string]string // extra headers to set; empty = none
	NoRequestID bool              // if true, don't auto-inject X-Request-Id
}

// ProfileVars holds runtime values available to User-Agent templates.
type ProfileVars struct {
	OS        string // runtime.GOOS, e.g. "linux", "darwin"
	Arch      string // normalised arch, e.g. "x64", "arm64"
	Platform  string // same as OS (alias used by some tools)
	OSVersion string // placeholder; set to "" for now
	Model     string // the model being requested
	SessionID string // stable per-process UUID
	Version   string // CLI version string, e.g. "0.40.0"
}

// sessionID is generated once per process and reused for all requests.
var sessionID = uuid.New().String()

const geminiCLIVersion = "0.40.0"

// DefaultVars returns ProfileVars populated with the current runtime environment.
func DefaultVars(model string) ProfileVars {
	return ProfileVars{
		OS:        runtime.GOOS,
		Arch:      normaliseArch(runtime.GOARCH),
		Platform:  runtime.GOOS,
		OSVersion: "",
		Model:     model,
		SessionID: sessionID,
		Version:   geminiCLIVersion,
	}
}

func normaliseArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "386":
		return "x86"
	default:
		return goarch
	}
}

// RenderUserAgent evaluates the profile's User-Agent template with the given vars.
// Returns empty string if the profile has no User-Agent template.
func (p *Profile) RenderUserAgent(vars ProfileVars) string {
	if p.UserAgent == "" {
		return ""
	}
	ua, err := renderTemplate(p.UserAgent, vars)
	if err != nil {
		return p.UserAgent // fallback to raw string on error
	}
	return ua
}

var tmplCache sync.Map // map[string]*template.Template

func renderTemplate(tmplStr string, vars ProfileVars) (string, error) {
	var tmpl *template.Template
	if cached, ok := tmplCache.Load(tmplStr); ok {
		tmpl = cached.(*template.Template)
	} else {
		var err error
		tmpl, err = template.New("").Parse(tmplStr)
		if err != nil {
			return "", err
		}
		tmplCache.Store(tmplStr, tmpl)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ProfileNoneID is the special "no spoofing" profile that preserves default behaviour.
const ProfileNoneID = "none"

// builtinProfiles contains all predefined identity profiles.
var builtinProfiles = []Profile{
	{
		ID:          ProfileNoneID,
		DisplayName: "None (passthrough)",
		UserAgent:   "",
		Headers:     nil,
	},
	{
		ID:          "codex-cli",
		DisplayName: "Codex CLI",
		UserAgent:   "codex_cli_rs/0.121.0 ({{.OS}}; {{.Arch}})",
		Headers: map[string]string{
			"originator": "codex_cli_rs",
		},
	},
	{
		ID:          "gemini-cli",
		DisplayName: "Gemini CLI",
		UserAgent:   "GeminiCLI/{{.Version}}/{{.Model}} ({{.Platform}}; {{.Arch}}; terminal)",
		Headers:     nil,
		NoRequestID: true,
	},
	{
		ID:          "copilot-vscode",
		DisplayName: "GitHub Copilot (VS Code)",
		UserAgent:   "GitHubCopilot/1.0",
		Headers: map[string]string{
			"Editor-Version":         "vscode/1.115.0",
			"Editor-Plugin-Version":  "copilot-chat/0.43.0",
			"Copilot-Integration-Id": "vscode-chat",
		},
	},
	{
		ID:          "opencode",
		DisplayName: "OpenCode CLI",
		UserAgent:   "opencode/1.4.6 ({{.Platform}}; {{.Arch}})",
		Headers: map[string]string{
			"HTTP-Referer": "https://opencode.ai/",
			"X-Title":      "opencode",
		},
	},
	{
		ID:          "claude-code",
		DisplayName: "Claude Code CLI",
		UserAgent:   "claude-cli/2.1.112 (pro, cli)",
		Headers: map[string]string{
			"x-app":                    "cli",
			"X-Claude-Code-Session-Id": "{{.SessionID}}",
		},
	},
}

// builtinByID is a lookup map built at init time.
var builtinByID map[string]*Profile

func init() {
	builtinByID = make(map[string]*Profile, len(builtinProfiles))
	for i := range builtinProfiles {
		builtinByID[builtinProfiles[i].ID] = &builtinProfiles[i]
	}
}

// Get returns a builtin profile by ID, or nil if not found.
func Get(id string) *Profile {
	return builtinByID[id]
}

// List returns all builtin profiles (including "none").
func List() []Profile {
	out := make([]Profile, len(builtinProfiles))
	copy(out, builtinProfiles)
	return out
}

// IsBuiltin returns true if the given ID is a predefined profile.
func IsBuiltin(id string) bool {
	_, ok := builtinByID[id]
	return ok
}

// Resolve looks up a profile by ID, first checking custom profiles, then builtins.
// Returns the "none" profile if id is empty or not found.
func Resolve(id string, custom []Profile) *Profile {
	if id == "" {
		return builtinByID[ProfileNoneID]
	}
	for i := range custom {
		if custom[i].ID == id {
			return &custom[i]
		}
	}
	if p := builtinByID[id]; p != nil {
		return p
	}
	return builtinByID[ProfileNoneID]
}

// AllProfiles returns builtins + custom profiles combined, for UI selectors.
func AllProfiles(custom []Profile) []Profile {
	all := List()
	all = append(all, custom...)
	return all
}
