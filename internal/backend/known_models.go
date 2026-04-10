package backend

import "strings"

// knownModelInfo holds default metadata for well-known models.
type knownModelInfo struct {
	DisplayName     string // human-readable name, e.g. "Claude Sonnet 4"
	ContextLength   int64
	MaxOutputTokens int64
	Vision          bool
}

// knownModels maps lowercase model ID prefixes to their metadata.
// Entries are matched by longest-prefix-first using prefixMatch.
var knownModels = map[string]knownModelInfo{
	// ── OpenAI ──────────────────────────────────────────────────
	"gpt-4o-":           {DisplayName: "GPT-4o", ContextLength: 128000, MaxOutputTokens: 16384, Vision: true},
	"gpt-4o":            {DisplayName: "GPT-4o", ContextLength: 128000, MaxOutputTokens: 16384, Vision: true},
	"gpt-4-turbo":       {DisplayName: "GPT-4 Turbo", ContextLength: 128000, MaxOutputTokens: 4096},
	"gpt-4-":            {DisplayName: "GPT-4", ContextLength: 128000, MaxOutputTokens: 4096},
	"gpt-4":             {DisplayName: "GPT-4", ContextLength: 8192, MaxOutputTokens: 8192},
	"gpt-4.1-mini-":     {DisplayName: "GPT-4.1 Mini", ContextLength: 1047576, MaxOutputTokens: 32768},
	"gpt-4.1-nano-":     {DisplayName: "GPT-4.1 Nano", ContextLength: 1047576, MaxOutputTokens: 32768},
	"gpt-4.1-":          {DisplayName: "GPT-4.1", ContextLength: 1047576, MaxOutputTokens: 32768},
	"gpt-4.1-mini":      {DisplayName: "GPT-4.1 Mini", ContextLength: 1047576, MaxOutputTokens: 32768},
	"gpt-4.1-nano":      {DisplayName: "GPT-4.1 Nano", ContextLength: 1047576, MaxOutputTokens: 32768},
	"gpt-4.1":           {DisplayName: "GPT-4.1", ContextLength: 1047576, MaxOutputTokens: 32768},
	"o3-":               {DisplayName: "o3", ContextLength: 200000, MaxOutputTokens: 100000},
	"o3":                {DisplayName: "o3", ContextLength: 200000, MaxOutputTokens: 100000},
	"o4-mini-":          {DisplayName: "o4 Mini", ContextLength: 200000, MaxOutputTokens: 100000},
	"o4-mini":           {DisplayName: "o4 Mini", ContextLength: 200000, MaxOutputTokens: 100000},
	"gpt-3.5-turbo-":    {DisplayName: "GPT-3.5 Turbo", ContextLength: 16385, MaxOutputTokens: 4096},
	"gpt-3.5-turbo":     {DisplayName: "GPT-3.5 Turbo", ContextLength: 16385, MaxOutputTokens: 4096},
	"chatgpt-4o-":       {DisplayName: "ChatGPT-4o", ContextLength: 128000, MaxOutputTokens: 16384, Vision: true},
	"chatgpt-4o":        {DisplayName: "ChatGPT-4o", ContextLength: 128000, MaxOutputTokens: 16384, Vision: true},

	// ── Anthropic Claude ────────────────────────────────────────
	"claude-sonnet-4-":  {DisplayName: "Claude Sonnet 4", ContextLength: 200000, MaxOutputTokens: 64000, Vision: true},
	"claude-sonnet-4":   {DisplayName: "Claude Sonnet 4", ContextLength: 200000, MaxOutputTokens: 64000, Vision: true},
	"claude-opus-4-":    {DisplayName: "Claude Opus 4", ContextLength: 200000, MaxOutputTokens: 32000, Vision: true},
	"claude-opus-4":     {DisplayName: "Claude Opus 4", ContextLength: 200000, MaxOutputTokens: 32000, Vision: true},
	"claude-3.7-sonnet-": {DisplayName: "Claude 3.7 Sonnet", ContextLength: 200000, MaxOutputTokens: 64000, Vision: true},
	"claude-3.7-sonnet": {DisplayName: "Claude 3.7 Sonnet", ContextLength: 200000, MaxOutputTokens: 64000, Vision: true},
	"claude-3.5-sonnet-": {DisplayName: "Claude 3.5 Sonnet", ContextLength: 200000, MaxOutputTokens: 8192, Vision: true},
	"claude-3.5-sonnet": {DisplayName: "Claude 3.5 Sonnet", ContextLength: 200000, MaxOutputTokens: 8192, Vision: true},
	"claude-3.5-haiku-": {DisplayName: "Claude 3.5 Haiku", ContextLength: 200000, MaxOutputTokens: 8192, Vision: true},
	"claude-3.5-haiku":  {DisplayName: "Claude 3.5 Haiku", ContextLength: 200000, MaxOutputTokens: 8192, Vision: true},
	"claude-3-opus-":    {DisplayName: "Claude 3 Opus", ContextLength: 200000, MaxOutputTokens: 4096, Vision: true},
	"claude-3-opus":     {DisplayName: "Claude 3 Opus", ContextLength: 200000, MaxOutputTokens: 4096, Vision: true},
	"claude-3-haiku-":   {DisplayName: "Claude 3 Haiku", ContextLength: 200000, MaxOutputTokens: 4096, Vision: true},
	"claude-3-haiku":    {DisplayName: "Claude 3 Haiku", ContextLength: 200000, MaxOutputTokens: 4096, Vision: true},

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
	"mistral-large-":    {DisplayName: "Mistral Large", ContextLength: 128000, MaxOutputTokens: 8192},
	"mistral-large":     {DisplayName: "Mistral Large", ContextLength: 128000, MaxOutputTokens: 8192},
	"mistral-medium-":   {DisplayName: "Mistral Medium", ContextLength: 32000, MaxOutputTokens: 8192},
	"mistral-small-":    {DisplayName: "Mistral Small", ContextLength: 128000, MaxOutputTokens: 8192},
	"mistral-small":     {DisplayName: "Mistral Small", ContextLength: 128000, MaxOutputTokens: 8192},
	"mistral-nemo-":     {DisplayName: "Mistral Nemo", ContextLength: 128000, MaxOutputTokens: 8192},
	"mistral-nemo":      {DisplayName: "Mistral Nemo", ContextLength: 128000, MaxOutputTokens: 8192},
	"codestral-":        {DisplayName: "Codestral", ContextLength: 256000, MaxOutputTokens: 8192},
	"codestral":         {DisplayName: "Codestral", ContextLength: 256000, MaxOutputTokens: 8192},

	// ── ZhipuAI / Z.ai GLM ──────────────────────────────────────
	"glm-5.1":           {DisplayName: "GLM-5.1", ContextLength: 200000, MaxOutputTokens: 128000},
	"glm-5-turbo":       {DisplayName: "GLM-5 Turbo", ContextLength: 200000, MaxOutputTokens: 128000},
	"glm-5":             {DisplayName: "GLM-5", ContextLength: 200000, MaxOutputTokens: 128000},
	"glm-4.7":           {DisplayName: "GLM-4.7", ContextLength: 128000, MaxOutputTokens: 8192},
	"glm-4.6v":          {DisplayName: "GLM-4.6V", ContextLength: 128000, MaxOutputTokens: 8192, Vision: true},
	"glm-4.5-air":       {DisplayName: "GLM-4.5 Air", ContextLength: 128000, MaxOutputTokens: 8192},
	"glm-4-":            {DisplayName: "GLM-4", ContextLength: 128000, MaxOutputTokens: 8192},
	"glm-4":             {DisplayName: "GLM-4", ContextLength: 128000, MaxOutputTokens: 8192},
	"chatglm-":          {DisplayName: "ChatGLM", ContextLength: 32000, MaxOutputTokens: 4096},

	// ── Alibaba Qwen ────────────────────────────────────────────
	"qwen3.6-plus":      {DisplayName: "Qwen 3.6 Plus", ContextLength: 131072, MaxOutputTokens: 16384},
	"qwen3-":            {DisplayName: "Qwen 3", ContextLength: 131072, MaxOutputTokens: 8192},
	"qwen3":             {DisplayName: "Qwen 3", ContextLength: 131072, MaxOutputTokens: 8192},
	"qwen2.5-72b-":      {DisplayName: "Qwen 2.5 72B", ContextLength: 131072, MaxOutputTokens: 8192},
	"qwen2.5-":          {DisplayName: "Qwen 2.5", ContextLength: 131072, MaxOutputTokens: 8192},
	"qwen2-":            {DisplayName: "Qwen 2", ContextLength: 32768, MaxOutputTokens: 8192},
	"qwen-":             {DisplayName: "Qwen", ContextLength: 32768, MaxOutputTokens: 8192},

	// ── DeepSeek ────────────────────────────────────────────────
	"deepseek-r1-":      {DisplayName: "DeepSeek R1", ContextLength: 131072, MaxOutputTokens: 16384},
	"deepseek-r1":       {DisplayName: "DeepSeek R1", ContextLength: 131072, MaxOutputTokens: 16384},
	"deepseek-v3-":      {DisplayName: "DeepSeek V3", ContextLength: 131072, MaxOutputTokens: 8192},
	"deepseek-v3":       {DisplayName: "DeepSeek V3", ContextLength: 131072, MaxOutputTokens: 8192},
	"deepseek-chat-":    {DisplayName: "DeepSeek Chat", ContextLength: 131072, MaxOutputTokens: 8192},
	"deepseek-chat":     {DisplayName: "DeepSeek Chat", ContextLength: 131072, MaxOutputTokens: 8192},
	"deepseek-coder-":   {DisplayName: "DeepSeek Coder", ContextLength: 131072, MaxOutputTokens: 8192},
	"deepseek-coder":    {DisplayName: "DeepSeek Coder", ContextLength: 131072, MaxOutputTokens: 8192},

	// ── Moonshot / Kimi ─────────────────────────────────────────
	"kimi-k2.5":         {DisplayName: "Kimi K2.5", ContextLength: 131072, MaxOutputTokens: 8192},
	"kimi-k2-":          {DisplayName: "Kimi K2", ContextLength: 131072, MaxOutputTokens: 8192},
	"kimi-k2":           {DisplayName: "Kimi K2", ContextLength: 131072, MaxOutputTokens: 8192},
	"moonshot-v1-":      {DisplayName: "Moonshot v1", ContextLength: 128000, MaxOutputTokens: 8192},
	"moonshot-v1":       {DisplayName: "Moonshot v1", ContextLength: 128000, MaxOutputTokens: 8192},

	// ── MiniMax ─────────────────────────────────────────────────
	"minimax-m2.7":      {DisplayName: "MiniMax M2.7", ContextLength: 1048576, MaxOutputTokens: 16384},
	"minimax-m2.5":      {DisplayName: "MiniMax M2.5", ContextLength: 1048576, MaxOutputTokens: 16384},
	"minimax-m2-":       {DisplayName: "MiniMax M2", ContextLength: 1048576, MaxOutputTokens: 16384},
	"minimax-m1-":       {DisplayName: "MiniMax M1", ContextLength: 1048576, MaxOutputTokens: 16384},
	"minimax-":          {DisplayName: "MiniMax", ContextLength: 245000, MaxOutputTokens: 8192},

	// ── xAI Grok ────────────────────────────────────────────────
	"grok-3-":           {DisplayName: "Grok 3", ContextLength: 131072, MaxOutputTokens: 32768},
	"grok-3":            {DisplayName: "Grok 3", ContextLength: 131072, MaxOutputTokens: 32768},
	"grok-2-":           {DisplayName: "Grok 2", ContextLength: 131072, MaxOutputTokens: 32768},
	"grok-2":            {DisplayName: "Grok 2", ContextLength: 131072, MaxOutputTokens: 32768},

	// ── Cohere ──────────────────────────────────────────────────
	"command-r-plus-":   {DisplayName: "Command R+", ContextLength: 128000, MaxOutputTokens: 4096},
	"command-r-plus":    {DisplayName: "Command R+", ContextLength: 128000, MaxOutputTokens: 4096},
	"command-r-":        {DisplayName: "Command R", ContextLength: 128000, MaxOutputTokens: 4096},
	"command-r":         {DisplayName: "Command R", ContextLength: 128000, MaxOutputTokens: 4096},

	// ── Other models ────────────────────────────────────────────
	"big-pickle":        {DisplayName: "Big Pickle", ContextLength: 131072, MaxOutputTokens: 8192},
	"nemotron-3-super":  {DisplayName: "Nemotron 3 Super", ContextLength: 131072, MaxOutputTokens: 8192},
	"mimo-v2-pro":       {DisplayName: "MIMO v2 Pro", ContextLength: 131072, MaxOutputTokens: 8192},
	"mimo-v2-omni":      {DisplayName: "MIMO v2 Omni", ContextLength: 131072, MaxOutputTokens: 8192, Vision: true},
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
