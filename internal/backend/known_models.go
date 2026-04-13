package backend

import "strings"

// knownModelInfo holds default metadata for well-known models.
type knownModelInfo struct {
	DisplayName     string // human-readable name, e.g. "Claude Sonnet 4"
	ContextLength   int64
	MaxOutputTokens int64
	Vision          bool
	// UseMaxCompletionTokens indicates that the model requires max_completion_tokens
	// instead of the legacy max_tokens parameter (OpenAI o-series, gpt-5.x).
	UseMaxCompletionTokens bool
	// SupportsSampling indicates that the model supports temperature/top_p parameters.
	// Reasoning models (o-series, gpt-5.x) do not support these.
	SupportsSampling bool
}

// knownModels maps lowercase model ID prefixes to their metadata.
// Entries are matched by longest-prefix-first using prefixMatch.
var knownModels = map[string]knownModelInfo{
	// ── OpenAI ──────────────────────────────────────────────────
	"gpt-4o-":        {DisplayName: "GPT-4o", ContextLength: 128000, MaxOutputTokens: 16384, Vision: true},
	"gpt-4o":         {DisplayName: "GPT-4o", ContextLength: 128000, MaxOutputTokens: 16384, Vision: true},
	"gpt-4-turbo":    {DisplayName: "GPT-4 Turbo", ContextLength: 128000, MaxOutputTokens: 4096},
	"gpt-4-":         {DisplayName: "GPT-4", ContextLength: 128000, MaxOutputTokens: 4096},
	"gpt-4":          {DisplayName: "GPT-4", ContextLength: 8192, MaxOutputTokens: 8192},
	"gpt-4.1-mini-":  {DisplayName: "GPT-4.1 Mini", ContextLength: 1047576, MaxOutputTokens: 32768},
	"gpt-4.1-nano-":  {DisplayName: "GPT-4.1 Nano", ContextLength: 1047576, MaxOutputTokens: 32768},
	"gpt-4.1-":       {DisplayName: "GPT-4.1", ContextLength: 1047576, MaxOutputTokens: 32768},
	"gpt-4.1-mini":   {DisplayName: "GPT-4.1 Mini", ContextLength: 1047576, MaxOutputTokens: 32768},
	"gpt-4.1-nano":   {DisplayName: "GPT-4.1 Nano", ContextLength: 1047576, MaxOutputTokens: 32768},
	"gpt-4.1":        {DisplayName: "GPT-4.1", ContextLength: 1047576, MaxOutputTokens: 32768},
	"o3-":            {DisplayName: "o3", ContextLength: 200000, MaxOutputTokens: 100000, UseMaxCompletionTokens: true, SupportsSampling: false},
	"o3":             {DisplayName: "o3", ContextLength: 200000, MaxOutputTokens: 100000, UseMaxCompletionTokens: true, SupportsSampling: false},
	"o4-mini-":       {DisplayName: "o4 Mini", ContextLength: 200000, MaxOutputTokens: 100000, UseMaxCompletionTokens: true, SupportsSampling: false},
	"o4-mini":        {DisplayName: "o4 Mini", ContextLength: 200000, MaxOutputTokens: 100000, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-3.5-turbo-": {DisplayName: "GPT-3.5 Turbo", ContextLength: 16385, MaxOutputTokens: 4096},
	"gpt-3.5-turbo":  {DisplayName: "GPT-3.5 Turbo", ContextLength: 16385, MaxOutputTokens: 4096},
	"chatgpt-4o-":    {DisplayName: "ChatGPT-4o", ContextLength: 128000, MaxOutputTokens: 16384, Vision: true},
	"chatgpt-4o":     {DisplayName: "ChatGPT-4o", ContextLength: 128000, MaxOutputTokens: 16384, Vision: true},

	// GPT-5.4 series — flagship 2026 lineup (context 1.05M or 400K)
	"gpt-5.4-pro-":  {DisplayName: "GPT-5.4 Pro", ContextLength: 1050000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5.4-pro":   {DisplayName: "GPT-5.4 Pro", ContextLength: 1050000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5.4-mini-": {DisplayName: "GPT-5.4 Mini", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5.4-mini":  {DisplayName: "GPT-5.4 Mini", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5.4-nano-": {DisplayName: "GPT-5.4 Nano", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5.4-nano":  {DisplayName: "GPT-5.4 Nano", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5.4-":      {DisplayName: "GPT-5.4", ContextLength: 1050000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5.4":       {DisplayName: "GPT-5.4", ContextLength: 1050000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},

	// GPT-5.3 series
	"gpt-5.3-codex-": {DisplayName: "GPT-5.3 Codex", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5.3-codex":  {DisplayName: "GPT-5.3 Codex", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5.3-":       {DisplayName: "GPT-5.3", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5.3":        {DisplayName: "GPT-5.3", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},

	// GPT-5.2 series
	"gpt-5.2-codex-": {DisplayName: "GPT-5.2 Codex", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5.2-codex":  {DisplayName: "GPT-5.2 Codex", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5.2-":       {DisplayName: "GPT-5.2", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5.2":        {DisplayName: "GPT-5.2", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},

	// GPT-5.1 series
	"gpt-5.1-codex-max-":  {DisplayName: "GPT-5.1 Codex Max", ContextLength: 400000, MaxOutputTokens: 128000, Vision: true, UseMaxCompletionTokens: true},
	"gpt-5.1-codex-max":   {DisplayName: "GPT-5.1 Codex Max", ContextLength: 400000, MaxOutputTokens: 128000, Vision: true, UseMaxCompletionTokens: true},
	"gpt-5.1-codex-mini-": {DisplayName: "GPT-5.1 Codex Mini", ContextLength: 400000, MaxOutputTokens: 128000, Vision: true, UseMaxCompletionTokens: true},
	"gpt-5.1-codex-mini":  {DisplayName: "GPT-5.1 Codex Mini", ContextLength: 400000, MaxOutputTokens: 128000, Vision: true, UseMaxCompletionTokens: true},
	"gpt-5.1-codex-":      {DisplayName: "GPT-5.1 Codex", ContextLength: 400000, MaxOutputTokens: 128000, Vision: true, UseMaxCompletionTokens: true},
	"gpt-5.1-codex":       {DisplayName: "GPT-5.1 Codex", ContextLength: 400000, MaxOutputTokens: 128000, Vision: true, UseMaxCompletionTokens: true},
	"gpt-5.1-":            {DisplayName: "GPT-5.1", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true},
	"gpt-5.1":             {DisplayName: "GPT-5.1", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true},

	// GPT-5 base series
	"gpt-5-codex-":      {DisplayName: "GPT-5 Codex", ContextLength: 400000, MaxOutputTokens: 128000, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5-codex":       {DisplayName: "GPT-5 Codex", ContextLength: 400000, MaxOutputTokens: 128000, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5-mini-":       {DisplayName: "GPT-5 Mini", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5-mini":        {DisplayName: "GPT-5 Mini", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5-nano-":       {DisplayName: "GPT-5 Nano", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5-nano":        {DisplayName: "GPT-5 Nano", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5-":            {DisplayName: "GPT-5", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"gpt-5":             {DisplayName: "GPT-5", ContextLength: 400000, MaxOutputTokens: 131072, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},
	"codex-mini-latest": {DisplayName: "Codex Mini Latest", ContextLength: 200000, MaxOutputTokens: 100000, Vision: true, UseMaxCompletionTokens: true, SupportsSampling: false},

	// ── Anthropic Claude ────────────────────────────────────────
	// Claude 4 series: Opus/Sonnet have 1M ctx; Haiku has 200K ctx.
	// API IDs use dash notation: claude-opus-4-6, claude-sonnet-4-6, claude-haiku-4-5.
	// Prefix "claude-opus-4-" matches all snapshots (e.g. claude-opus-4-6-20260115).
	"claude-opus-4-":   {DisplayName: "Claude Opus 4", ContextLength: 1000000, MaxOutputTokens: 131072, Vision: true},
	"claude-opus-4":    {DisplayName: "Claude Opus 4", ContextLength: 1000000, MaxOutputTokens: 131072, Vision: true},
	"claude-sonnet-4-": {DisplayName: "Claude Sonnet 4", ContextLength: 1000000, MaxOutputTokens: 65536, Vision: true},
	"claude-sonnet-4":  {DisplayName: "Claude Sonnet 4", ContextLength: 1000000, MaxOutputTokens: 65536, Vision: true},
	"claude-haiku-4-":  {DisplayName: "Claude Haiku 4", ContextLength: 200000, MaxOutputTokens: 65536, Vision: true},
	"claude-haiku-4":   {DisplayName: "Claude Haiku 4", ContextLength: 200000, MaxOutputTokens: 65536, Vision: true},

	// Claude 3.7 series: 200K ctx; 64K output via extended thinking
	"claude-3.7-sonnet-": {DisplayName: "Claude 3.7 Sonnet", ContextLength: 200000, MaxOutputTokens: 65536, Vision: true},
	"claude-3.7-sonnet":  {DisplayName: "Claude 3.7 Sonnet", ContextLength: 200000, MaxOutputTokens: 65536, Vision: true},

	// Claude 3.5 series
	"claude-3.5-sonnet-": {DisplayName: "Claude 3.5 Sonnet", ContextLength: 200000, MaxOutputTokens: 8192, Vision: true},
	"claude-3.5-sonnet":  {DisplayName: "Claude 3.5 Sonnet", ContextLength: 200000, MaxOutputTokens: 8192, Vision: true},
	"claude-3.5-haiku-":  {DisplayName: "Claude 3.5 Haiku", ContextLength: 200000, MaxOutputTokens: 8192, Vision: true},
	"claude-3.5-haiku":   {DisplayName: "Claude 3.5 Haiku", ContextLength: 200000, MaxOutputTokens: 8192, Vision: true},

	// Claude 3 series
	"claude-3-opus-":   {DisplayName: "Claude 3 Opus", ContextLength: 200000, MaxOutputTokens: 4096, Vision: true},
	"claude-3-opus":    {DisplayName: "Claude 3 Opus", ContextLength: 200000, MaxOutputTokens: 4096, Vision: true},
	"claude-3-sonnet-": {DisplayName: "Claude 3 Sonnet", ContextLength: 200000, MaxOutputTokens: 4096, Vision: true},
	"claude-3-sonnet":  {DisplayName: "Claude 3 Sonnet", ContextLength: 200000, MaxOutputTokens: 4096, Vision: true},
	"claude-3-haiku-":  {DisplayName: "Claude 3 Haiku", ContextLength: 200000, MaxOutputTokens: 4096, Vision: true},
	"claude-3-haiku":   {DisplayName: "Claude 3 Haiku", ContextLength: 200000, MaxOutputTokens: 4096, Vision: true},

	// ── Google Gemini ───────────────────────────────────────────
	"gemini-3.1-pro-":   {DisplayName: "Gemini 3.1 Pro", ContextLength: 1048576, MaxOutputTokens: 65536, Vision: true},
	"gemini-3.1-pro":    {DisplayName: "Gemini 3.1 Pro", ContextLength: 1048576, MaxOutputTokens: 65536, Vision: true},
	"gemini-2.5-pro-":   {DisplayName: "Gemini 2.5 Pro", ContextLength: 1048576, MaxOutputTokens: 65536, Vision: true},
	"gemini-2.5-pro":    {DisplayName: "Gemini 2.5 Pro", ContextLength: 1048576, MaxOutputTokens: 65536, Vision: true},
	"gemini-2.5-flash-": {DisplayName: "Gemini 2.5 Flash", ContextLength: 1048576, MaxOutputTokens: 65536, Vision: true},
	"gemini-2.5-flash":  {DisplayName: "Gemini 2.5 Flash", ContextLength: 1048576, MaxOutputTokens: 65536, Vision: true},
	"gemini-2.0-flash-": {DisplayName: "Gemini 2.0 Flash", ContextLength: 1048576, MaxOutputTokens: 8192, Vision: true},
	"gemini-2.0-flash":  {DisplayName: "Gemini 2.0 Flash", ContextLength: 1048576, MaxOutputTokens: 8192, Vision: true},
	"gemini-1.5-pro-":   {DisplayName: "Gemini 1.5 Pro", ContextLength: 2097152, MaxOutputTokens: 8192, Vision: true},
	"gemini-1.5-pro":    {DisplayName: "Gemini 1.5 Pro", ContextLength: 2097152, MaxOutputTokens: 8192, Vision: true},
	"gemini-1.5-flash-": {DisplayName: "Gemini 1.5 Flash", ContextLength: 1048576, MaxOutputTokens: 8192, Vision: true},
	"gemini-1.5-flash":  {DisplayName: "Gemini 1.5 Flash", ContextLength: 1048576, MaxOutputTokens: 8192, Vision: true},

	// ── Meta Llama ──────────────────────────────────────────────
	"llama-4-maverick-": {DisplayName: "Llama 4 Maverick", ContextLength: 1048576, MaxOutputTokens: 16384},
	"llama-4-maverick":  {DisplayName: "Llama 4 Maverick", ContextLength: 1048576, MaxOutputTokens: 16384},
	"llama-4-scout-":    {DisplayName: "Llama 4 Scout", ContextLength: 1048576, MaxOutputTokens: 16384},
	"llama-4-scout":     {DisplayName: "Llama 4 Scout", ContextLength: 1048576, MaxOutputTokens: 16384},
	"llama-3.3-70b-":    {DisplayName: "Llama 3.3 70B", ContextLength: 128000, MaxOutputTokens: 16384},
	"llama-3.3-70b":     {DisplayName: "Llama 3.3 70B", ContextLength: 128000, MaxOutputTokens: 16384},
	"llama-3.1-405b-":   {DisplayName: "Llama 3.1 405B", ContextLength: 128000, MaxOutputTokens: 16384},
	"llama-3.1-405b":    {DisplayName: "Llama 3.1 405B", ContextLength: 128000, MaxOutputTokens: 16384},
	"llama-3.1-70b-":    {DisplayName: "Llama 3.1 70B", ContextLength: 128000, MaxOutputTokens: 16384},
	"llama-3.1-70b":     {DisplayName: "Llama 3.1 70B", ContextLength: 128000, MaxOutputTokens: 16384},
	"llama-3.1-8b-":     {DisplayName: "Llama 3.1 8B", ContextLength: 128000, MaxOutputTokens: 16384},
	"llama-3.1-8b":      {DisplayName: "Llama 3.1 8B", ContextLength: 128000, MaxOutputTokens: 16384},
	"llama-3-":          {DisplayName: "Llama 3", ContextLength: 8192, MaxOutputTokens: 4096},
	"llama-3":           {DisplayName: "Llama 3", ContextLength: 8192, MaxOutputTokens: 4096},

	// ── Mistral ─────────────────────────────────────────────────
	"mistral-large-":  {DisplayName: "Mistral Large", ContextLength: 128000, MaxOutputTokens: 8192},
	"mistral-large":   {DisplayName: "Mistral Large", ContextLength: 128000, MaxOutputTokens: 8192},
	"mistral-medium-": {DisplayName: "Mistral Medium", ContextLength: 32000, MaxOutputTokens: 8192},
	"mistral-small-":  {DisplayName: "Mistral Small", ContextLength: 128000, MaxOutputTokens: 8192},
	"mistral-small":   {DisplayName: "Mistral Small", ContextLength: 128000, MaxOutputTokens: 8192},
	"mistral-nemo-":   {DisplayName: "Mistral Nemo", ContextLength: 128000, MaxOutputTokens: 8192},
	"mistral-nemo":    {DisplayName: "Mistral Nemo", ContextLength: 128000, MaxOutputTokens: 8192},
	"codestral-":      {DisplayName: "Codestral", ContextLength: 256000, MaxOutputTokens: 8192},
	"codestral":       {DisplayName: "Codestral", ContextLength: 256000, MaxOutputTokens: 8192},

	// ── ZhipuAI / Z.ai GLM ──────────────────────────────────────
	"glm-5.1":     {DisplayName: "GLM-5.1", ContextLength: 200000, MaxOutputTokens: 128000},
	"glm-5-turbo": {DisplayName: "GLM-5 Turbo", ContextLength: 200000, MaxOutputTokens: 128000},
	"glm-5":       {DisplayName: "GLM-5", ContextLength: 200000, MaxOutputTokens: 128000},
	"glm-4.7":     {DisplayName: "GLM-4.7", ContextLength: 128000, MaxOutputTokens: 8192},
	"glm-4.6v":    {DisplayName: "GLM-4.6V", ContextLength: 128000, MaxOutputTokens: 8192, Vision: true},
	"glm-4.5-air": {DisplayName: "GLM-4.5 Air", ContextLength: 128000, MaxOutputTokens: 8192},
	"glm-4-":      {DisplayName: "GLM-4", ContextLength: 128000, MaxOutputTokens: 8192},
	"glm-4":       {DisplayName: "GLM-4", ContextLength: 128000, MaxOutputTokens: 8192},
	"chatglm-":    {DisplayName: "ChatGLM", ContextLength: 32000, MaxOutputTokens: 4096},

	// ── Alibaba Qwen ────────────────────────────────────────────
	"qwen3.6-plus": {DisplayName: "Qwen 3.6 Plus", ContextLength: 131072, MaxOutputTokens: 16384},
	"qwen3-":       {DisplayName: "Qwen 3", ContextLength: 131072, MaxOutputTokens: 8192},
	"qwen3":        {DisplayName: "Qwen 3", ContextLength: 131072, MaxOutputTokens: 8192},
	"qwen2.5-72b-": {DisplayName: "Qwen 2.5 72B", ContextLength: 131072, MaxOutputTokens: 8192},
	"qwen2.5-":     {DisplayName: "Qwen 2.5", ContextLength: 131072, MaxOutputTokens: 8192},
	"qwen2-":       {DisplayName: "Qwen 2", ContextLength: 32768, MaxOutputTokens: 8192},
	"qwen-":        {DisplayName: "Qwen", ContextLength: 32768, MaxOutputTokens: 8192},

	// ── DeepSeek ────────────────────────────────────────────────
	"deepseek-r1-":    {DisplayName: "DeepSeek R1", ContextLength: 131072, MaxOutputTokens: 16384},
	"deepseek-r1":     {DisplayName: "DeepSeek R1", ContextLength: 131072, MaxOutputTokens: 16384},
	"deepseek-v3-":    {DisplayName: "DeepSeek V3", ContextLength: 131072, MaxOutputTokens: 8192},
	"deepseek-v3":     {DisplayName: "DeepSeek V3", ContextLength: 131072, MaxOutputTokens: 8192},
	"deepseek-chat-":  {DisplayName: "DeepSeek Chat", ContextLength: 131072, MaxOutputTokens: 8192},
	"deepseek-chat":   {DisplayName: "DeepSeek Chat", ContextLength: 131072, MaxOutputTokens: 8192},
	"deepseek-coder-": {DisplayName: "DeepSeek Coder", ContextLength: 131072, MaxOutputTokens: 8192},
	"deepseek-coder":  {DisplayName: "DeepSeek Coder", ContextLength: 131072, MaxOutputTokens: 8192},

	// ── Moonshot / Kimi ─────────────────────────────────────────
	"kimi-k2.5":    {DisplayName: "Kimi K2.5", ContextLength: 131072, MaxOutputTokens: 8192},
	"kimi-k2-":     {DisplayName: "Kimi K2", ContextLength: 131072, MaxOutputTokens: 8192},
	"kimi-k2":      {DisplayName: "Kimi K2", ContextLength: 131072, MaxOutputTokens: 8192},
	"moonshot-v1-": {DisplayName: "Moonshot v1", ContextLength: 128000, MaxOutputTokens: 8192},
	"moonshot-v1":  {DisplayName: "Moonshot v1", ContextLength: 128000, MaxOutputTokens: 8192},

	// ── MiniMax ─────────────────────────────────────────────────
	"minimax-m2.7": {DisplayName: "MiniMax M2.7", ContextLength: 1048576, MaxOutputTokens: 16384},
	"minimax-m2.5": {DisplayName: "MiniMax M2.5", ContextLength: 1048576, MaxOutputTokens: 16384},
	"minimax-m2-":  {DisplayName: "MiniMax M2", ContextLength: 1048576, MaxOutputTokens: 16384},
	"minimax-m1-":  {DisplayName: "MiniMax M1", ContextLength: 1048576, MaxOutputTokens: 16384},
	"minimax-":     {DisplayName: "MiniMax", ContextLength: 245000, MaxOutputTokens: 8192},

	// ── xAI Grok ────────────────────────────────────────────────
	"grok-3-": {DisplayName: "Grok 3", ContextLength: 131072, MaxOutputTokens: 32768},
	"grok-3":  {DisplayName: "Grok 3", ContextLength: 131072, MaxOutputTokens: 32768},
	"grok-2-": {DisplayName: "Grok 2", ContextLength: 131072, MaxOutputTokens: 32768},
	"grok-2":  {DisplayName: "Grok 2", ContextLength: 131072, MaxOutputTokens: 32768},

	// ── Cohere ──────────────────────────────────────────────────
	"command-r-plus-": {DisplayName: "Command R+", ContextLength: 128000, MaxOutputTokens: 4096},
	"command-r-plus":  {DisplayName: "Command R+", ContextLength: 128000, MaxOutputTokens: 4096},
	"command-r-":      {DisplayName: "Command R", ContextLength: 128000, MaxOutputTokens: 4096},
	"command-r":       {DisplayName: "Command R", ContextLength: 128000, MaxOutputTokens: 4096},

	// ── Other models ────────────────────────────────────────────
	"big-pickle":       {DisplayName: "Big Pickle", ContextLength: 131072, MaxOutputTokens: 8192},
	"nemotron-3-super": {DisplayName: "Nemotron 3 Super", ContextLength: 131072, MaxOutputTokens: 8192},
	"mimo-v2-pro":      {DisplayName: "MIMO v2 Pro", ContextLength: 131072, MaxOutputTokens: 8192},
	"mimo-v2-omni":     {DisplayName: "MIMO v2 Omni", ContextLength: 131072, MaxOutputTokens: 8192, Vision: true},
}

// LookupKnownModel searches for a model ID in the built-in database.
// It uses longest-prefix matching: the most specific (longest) matching
// prefix wins.  Returns nil if no match is found.
func LookupKnownModel(modelID string) *knownModelInfo {
	id := strings.ToLower(modelID)
	var best string
	for prefix := range knownModels {
		if strings.HasPrefix(id, prefix) && len(prefix) > len(best) {
			best = prefix
		}
		// Also try matching the full ID exactly (without trailing dash).
		if prefix == id && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return nil
	}
	info := knownModels[best]
	return &info
}

// ApplyKnownDefaults fills missing Model fields from the built-in model database.
// This is a package-level helper used by multiple backend types.
func ApplyKnownDefaults(m *Model, modelID string) {
	applyKnownDefaults(m, modelID)
}

// applyKnownDefaults fills missing Model fields from the built-in model database.
func applyKnownDefaults(m *Model, modelID string) {
	info := LookupKnownModel(modelID)
	if info == nil {
		return
	}
	if m.DisplayName == "" && info.DisplayName != "" {
		m.DisplayName = info.DisplayName
	}
	if m.ContextLength == nil {
		m.ContextLength = &info.ContextLength
	}
	if m.MaxOutputTokens == nil {
		m.MaxOutputTokens = &info.MaxOutputTokens
	}
	if info.Vision {
		hasVision := false
		for _, c := range m.Capabilities {
			if c == "vision" {
				hasVision = true
				break
			}
		}
		if !hasVision {
			m.Capabilities = append(m.Capabilities, "vision")
		}
	}
}
