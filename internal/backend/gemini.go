package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/identity"
	"github.com/menno/llmapiproxy/internal/oauth"
	"github.com/rs/zerolog/log"
)

const (
	geminiCodeAssistBaseURL = "https://cloudcode-pa.googleapis.com/v1internal"
	geminiMaxAuthRetries    = 1
	geminiHTTPTimeout       = 5 * time.Minute
)

type GeminiBackend struct {
	name            string
	baseURL         string
	models          []string
	client          *http.Client
	oauthHandler    *oauth.GeminiOAuthHandler
	tokenStore      *oauth.TokenStore
	cfg             config.BackendConfig
	identityProfile *identity.Profile
	disabledModels  map[string]bool

	modelCacheTTL time.Duration
	cacheMu       sync.RWMutex
	cachedModels  []Model
	cacheExpiry   time.Time
	cacheStore    *ModelCacheStore

	onboardMu      sync.Mutex
	onboarded      bool
	cloudAIProject string
}

func NewGeminiBackend(cfg config.BackendConfig, oauthHandler *oauth.GeminiOAuthHandler, tokenStore *oauth.TokenStore, cacheTTL time.Duration, cacheStore *ModelCacheStore, profile *identity.Profile) *GeminiBackend {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = geminiCodeAssistBaseURL
	}

	dm := make(map[string]bool, len(cfg.DisabledModels))
	for _, m := range cfg.DisabledModels {
		dm[m] = true
	}

	return &GeminiBackend{
		name:            cfg.Name,
		baseURL:         baseURL,
		models:          cfg.ModelIDs(),
		client:          &http.Client{Timeout: geminiHTTPTimeout},
		oauthHandler:    oauthHandler,
		tokenStore:      tokenStore,
		cfg:             cfg,
		modelCacheTTL:   cacheTTL,
		disabledModels:  dm,
		cacheStore:      cacheStore,
		identityProfile: profile,
	}
}

func (b *GeminiBackend) Name() string { return b.name }

func (b *GeminiBackend) SupportsModel(modelID string) bool {
	if b.disabledModels[modelID] {
		return false
	}
	if len(b.models) > 0 {
		for _, m := range b.models {
			if strings.EqualFold(m, modelID) {
				return true
			}
		}
		return false
	}
	return true
}

func (b *GeminiBackend) ResolveModelID(canonicalID string) string { return canonicalID }

func (b *GeminiBackend) ClearModelCache() {
	b.cacheMu.Lock()
	b.cachedModels = nil
	b.cacheExpiry = time.Time{}
	b.cacheMu.Unlock()
}

// ResetOnboarding clears the onboarding state so that the next request
// will re-run loadCodeAssist and re-fetch the cloudAIProject. This should
// be called after a new OAuth token is obtained (fresh auth or re-auth).
func (b *GeminiBackend) ResetOnboarding() {
	b.onboardMu.Lock()
	b.onboarded = false
	b.cloudAIProject = ""
	b.onboardMu.Unlock()
}

func (b *GeminiBackend) ChatCompletion(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return b.doChatCompletion(ctx, req, 0)
}

func (b *GeminiBackend) doChatCompletion(ctx context.Context, req *ChatCompletionRequest, retryCount int) (*ChatCompletionResponse, error) {
	if len(req.Messages) == 0 {
		return nil, &BackendError{
			StatusCode: http.StatusBadRequest,
			Body:       `{"error":{"message":"messages array is empty","type":"invalid_request_error"}}`,
			Err:        fmt.Errorf("gemini backend %s: messages array is empty", b.name),
		}
	}

	token, err := b.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("gemini backend %s: %w", b.name, err)
	}

	if err := b.ensureOnboarded(ctx, token); err != nil {
		return nil, fmt.Errorf("gemini backend %s: onboarding: %w", b.name, err)
	}

	geminiReq := b.translateRequest(req)
	body, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, fmt.Errorf("gemini backend %s: marshaling request: %w", b.name, err)
	}

	endpoint := b.methodEndpoint("generateContent")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gemini backend %s: creating request: %w", b.name, err)
	}
	b.setHeaders(httpReq, token)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini backend %s: sending request: %w", b.name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized && retryCount < geminiMaxAuthRetries {
		if _, refreshErr := b.oauthHandler.RefreshToken(ctx); refreshErr != nil {
			return nil, fmt.Errorf("gemini backend %s: token refresh failed on 401: %w", b.name, refreshErr)
		}
		return b.doChatCompletion(ctx, req, retryCount+1)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, &BackendError{
			StatusCode: resp.StatusCode,
			Body:       string(errBody),
			Err:        fmt.Errorf("gemini backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody)),
		}
	}

	var caResp caGenerateContentResponse
	if err := json.NewDecoder(resp.Body).Decode(&caResp); err != nil {
		return nil, fmt.Errorf("gemini backend %s: decoding response: %w", b.name, err)
	}

	return translateFromGeminiResponse(&caResp, req.Model)
}

func (b *GeminiBackend) ChatCompletionStream(ctx context.Context, req *ChatCompletionRequest) (io.ReadCloser, error) {
	return b.doChatCompletionStream(ctx, req, 0)
}

func (b *GeminiBackend) doChatCompletionStream(ctx context.Context, req *ChatCompletionRequest, retryCount int) (io.ReadCloser, error) {
	if len(req.Messages) == 0 {
		return nil, &BackendError{
			StatusCode: http.StatusBadRequest,
			Body:       `{"error":{"message":"messages array is empty","type":"invalid_request_error"}}`,
			Err:        fmt.Errorf("gemini backend %s: messages array is empty", b.name),
		}
	}

	token, err := b.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("gemini backend %s: %w", b.name, err)
	}

	if err := b.ensureOnboarded(ctx, token); err != nil {
		return nil, fmt.Errorf("gemini backend %s: onboarding: %w", b.name, err)
	}

	geminiReq := b.translateRequest(req)
	body, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, fmt.Errorf("gemini backend %s: marshaling request: %w", b.name, err)
	}

	endpoint := b.methodEndpoint("streamGenerateContent?alt=sse")
	client := &http.Client{}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gemini backend %s: creating request: %w", b.name, err)
	}
	b.setHeaders(httpReq, token)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini backend %s: sending request: %w", b.name, err)
	}

	if resp.StatusCode == http.StatusUnauthorized && retryCount < geminiMaxAuthRetries {
		resp.Body.Close()
		if _, refreshErr := b.oauthHandler.RefreshToken(ctx); refreshErr != nil {
			return nil, fmt.Errorf("gemini backend %s: token refresh failed on 401: %w", b.name, refreshErr)
		}
		return b.doChatCompletionStream(ctx, req, retryCount+1)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &BackendError{
			StatusCode: resp.StatusCode,
			Body:       string(errBody),
			Err:        fmt.Errorf("gemini backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody)),
		}
	}

	return newGeminiStreamReader(resp.Body, uuid.New().String(), req.Model), nil
}

func (b *GeminiBackend) ListModels(ctx context.Context) ([]Model, error) {
	if b.modelCacheTTL > 0 {
		b.cacheMu.RLock()
		if !b.cacheExpiry.IsZero() && time.Now().Before(b.cacheExpiry) {
			cached := b.cachedModels
			b.cacheMu.RUnlock()
			return b.markDisabled(cached), nil
		}
		b.cacheMu.RUnlock()

		if b.cacheStore != nil {
			if models, expiry, ok := b.cacheStore.Load(b.name); ok && len(models) > 0 && time.Now().Before(expiry) {
				b.cacheMu.Lock()
				b.cachedModels = models
				b.cacheExpiry = expiry
				b.cacheMu.Unlock()
				return b.markDisabled(models), nil
			}
		}
	}

	models := b.buildModelList()

	if b.modelCacheTTL > 0 {
		b.cacheMu.Lock()
		b.cachedModels = models
		b.cacheExpiry = time.Now().Add(b.modelCacheTTL)
		b.cacheMu.Unlock()

		if b.cacheStore != nil {
			b.cacheStore.Save(b.name, models, b.cacheExpiry)
		}
	}

	return b.markDisabled(models), nil
}

func (b *GeminiBackend) buildModelList() []Model {
	configModels := b.cfg.ModelIDs()
	if len(configModels) > 0 {
		models := make([]Model, 0, len(configModels))
		for _, id := range configModels {
			models = append(models, Model{
				ID:      id,
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "google",
			})
		}
		return models
	}
	return []Model{
		{ID: "gemini-3.1-pro-preview", Object: "model", Created: time.Now().Unix(), OwnedBy: "google"},
		{ID: "gemini-3.1-flash-lite-preview", Object: "model", Created: time.Now().Unix(), OwnedBy: "google"},
		{ID: "gemini-3-pro-preview", Object: "model", Created: time.Now().Unix(), OwnedBy: "google"},
		{ID: "gemini-3-flash-preview", Object: "model", Created: time.Now().Unix(), OwnedBy: "google"},
		{ID: "gemini-2.5-pro", Object: "model", Created: time.Now().Unix(), OwnedBy: "google"},
		{ID: "gemini-2.5-flash", Object: "model", Created: time.Now().Unix(), OwnedBy: "google"},
		{ID: "gemini-2.5-flash-lite", Object: "model", Created: time.Now().Unix(), OwnedBy: "google"},
		{ID: "auto-gemini-3", Object: "model", Created: time.Now().Unix(), OwnedBy: "google"},
		{ID: "auto-gemini-2.5", Object: "model", Created: time.Now().Unix(), OwnedBy: "google"},
	}
}

func (b *GeminiBackend) markDisabled(models []Model) []Model {
	if len(b.disabledModels) == 0 {
		return models
	}
	out := make([]Model, len(models))
	for i, m := range models {
		out[i] = m
		if b.disabledModels[m.ID] {
			out[i].Disabled = true
		}
	}
	return out
}

func (b *GeminiBackend) methodEndpoint(method string) string {
	return b.baseURL + ":" + method
}

func (b *GeminiBackend) getAccessToken(ctx context.Context) (string, error) {
	token := b.tokenStore.ValidToken()
	if token != nil {
		return token.AccessToken, nil
	}

	tokenData, err := b.oauthHandler.RefreshWithRetry(ctx)
	if err != nil {
		return "", fmt.Errorf("Gemini authentication required; complete OAuth setup via the web UI: %w", err)
	}

	return tokenData.AccessToken, nil
}

func (b *GeminiBackend) setHeaders(httpReq *http.Request, accessToken string) {
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)

	if b.identityProfile != nil && b.identityProfile.ID != identity.ProfileNoneID {
		identity.ApplyProfile(httpReq, b.identityProfile, "")
	} else {
		httpReq.Header.Set("User-Agent", "GeminiCLI/0.40.0/ (linux; x64; terminal)")
	}
}

func (b *GeminiBackend) ensureOnboarded(ctx context.Context, accessToken string) error {
	b.onboardMu.Lock()
	defer b.onboardMu.Unlock()

	if b.onboarded {
		return nil
	}

	reqBody := map[string]interface{}{
		"metadata": map[string]interface{}{
			"ideType":    "IDE_UNSPECIFIED",
			"platform":   "PLATFORM_UNSPECIFIED",
			"pluginType": "GEMINI",
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshaling loadCodeAssist request: %w", err)
	}

	endpoint := b.methodEndpoint("loadCodeAssist")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating loadCodeAssist request: %w", err)
	}
	b.setHeaders(httpReq, accessToken)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("loadCodeAssist request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading loadCodeAssist response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		log.Warn().Int("status", resp.StatusCode).Str("body", string(respBody)).Msg("loadCodeAssist failed, continuing anyway")
		b.onboarded = true
		return nil
	}

	var loadResp map[string]interface{}
	if err := json.Unmarshal(respBody, &loadResp); err != nil {
		log.Warn().Err(err).Msg("failed to parse loadCodeAssist response, continuing anyway")
		b.onboarded = true
		return nil
	}

	if proj, ok := loadResp["cloudaicompanionProject"]; ok && proj != nil {
		switch p := proj.(type) {
		case string:
			b.cloudAIProject = p
		case map[string]interface{}:
			if id, ok := p["id"]; ok {
				b.cloudAIProject = fmt.Sprintf("%v", id)
			}
		}
	}

	if currentTier, ok := loadResp["currentTier"]; ok && currentTier != nil {
		log.Debug().Interface("tier", currentTier).Str("project", b.cloudAIProject).Msg("gemini code assist tier confirmed")
	} else {
		log.Info().Msg("no current tier found, attempting onboardUser")
		if err := b.doOnboardUser(ctx, accessToken); err != nil {
			log.Warn().Err(err).Msg("onboardUser failed, continuing anyway")
		}
	}

	b.onboarded = true
	return nil
}

func (b *GeminiBackend) doOnboardUser(ctx context.Context, accessToken string) error {
	reqBody := map[string]interface{}{
		"metadata": map[string]interface{}{
			"ideType":    "IDE_UNSPECIFIED",
			"platform":   "PLATFORM_UNSPECIFIED",
			"pluginType": "GEMINI",
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshaling onboardUser request: %w", err)
	}

	endpoint := b.methodEndpoint("onboardUser")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating onboardUser request: %w", err)
	}
	b.setHeaders(httpReq, accessToken)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("onboardUser request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		var onboardResp map[string]interface{}
		if err := json.Unmarshal(respBody, &onboardResp); err == nil {
			if name, ok := onboardResp["name"]; ok {
				log.Debug().Str("name", fmt.Sprintf("%v", name)).Msg("onboardUser LRO started")
			}
		}
		log.Info().Msg("gemini code assist onboarding completed")
		return nil
	}

	return fmt.Errorf("onboardUser returned status %d: %s", resp.StatusCode, string(respBody))
}

// --- OAuth interfaces ---

func (b *GeminiBackend) OAuthStatus() OAuthStatus {
	status := OAuthStatus{
		BackendName: b.name,
		BackendType: "gemini",
		TokenState:  "missing",
	}

	token := b.tokenStore.Get()
	if token != nil {
		status.Authenticated = !token.IsExpired()
		status.TokenSource = token.Source
		status.ExpiresAt = token.ExpiresAt
		status.ObtainedAt = token.ObtainedAt
		if !token.ExpiresAt.IsZero() {
			status.TokenExpiry = token.ExpiresAt.Format(time.RFC3339)
		}
		if !token.ObtainedAt.IsZero() {
			status.LastRefresh = token.ObtainedAt.Format(time.RFC3339)
		}
		status.NeedsReauth = token.IsExpired() && token.RefreshToken == ""
		if token.IsExpired() {
			status.TokenState = "expired"
		} else if time.Until(token.ExpiresAt) < 5*time.Minute {
			status.TokenState = "expiring"
		} else {
			status.TokenState = "valid"
		}
	}

	return status
}

func (b *GeminiBackend) InitiateLogin() (authURL string, state string, err error) {
	return b.oauthHandler.AuthorizeURL()
}

func (b *GeminiBackend) HandleCallback(ctx context.Context, code string, state string) error {
	_, err := b.oauthHandler.HandleCallback(ctx, code, state)
	return err
}

func (b *GeminiBackend) Disconnect() error {
	return b.tokenStore.Clear()
}

func (b *GeminiBackend) RefreshOAuthStatus(ctx context.Context) error {
	if b.oauthHandler == nil {
		return fmt.Errorf("gemini backend %s: oauth handler not configured", b.name)
	}
	_, err := b.oauthHandler.RefreshWithRetry(ctx)
	if err != nil {
		return fmt.Errorf("gemini backend %s: token refresh failed: %w", b.name, err)
	}
	return nil
}

func (b *GeminiBackend) GetOAuthHandler() *oauth.GeminiOAuthHandler {
	return b.oauthHandler
}

func (b *GeminiBackend) GetTokenStore() *oauth.TokenStore {
	return b.tokenStore
}

// --- Gemini Code Assist request/response types ---

type caGenerateContentRequest struct {
	Model              string          `json:"model"`
	Project            string          `json:"project,omitempty"`
	UserPromptID       string          `json:"user_prompt_id,omitempty"`
	Request            json.RawMessage `json:"request"`
	EnabledCreditTypes []string        `json:"enabled_credit_types,omitempty"`
}

type geminiContentRequest struct {
	Contents          []geminiContent  `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	Tools             []geminiTool     `json:"tools,omitempty"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
	SessionID         string           `json:"session_id,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	Thought          bool                    `json:"thought,omitempty"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations,omitempty"`
}

type geminiFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiGenConfig struct {
	Temperature     *float64              `json:"temperature,omitempty"`
	TopP            *float64              `json:"topP,omitempty"`
	TopK            *int                  `json:"topK,omitempty"`
	MaxOutputTokens *int                  `json:"maxOutputTokens,omitempty"`
	StopSequences   []string              `json:"stopSequences,omitempty"`
	ThinkingConfig  *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

type geminiThinkingConfig struct {
	IncludeThoughts bool `json:"includeThoughts,omitempty"`
}

type caGenerateContentResponse struct {
	Response         *geminiResponse `json:"response,omitempty"`
	TraceID          string          `json:"traceId,omitempty"`
	ConsumedCredits  json.RawMessage `json:"consumedCredits,omitempty"`
	RemainingCredits json.RawMessage `json:"remainingCredits,omitempty"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates,omitempty"`
	UsageMetadata *geminiUsage      `json:"usageMetadata,omitempty"`
	ModelVersion  string            `json:"modelVersion,omitempty"`
}

type geminiCandidate struct {
	Content      *geminiContent `json:"content,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
	Index        int            `json:"index,omitempty"`
}

type geminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// --- Request/Response translation ---

func (b *GeminiBackend) translateRequest(req *ChatCompletionRequest) *caGenerateContentRequest {
	var contents []geminiContent
	var systemInstruction *geminiContent
	var tools []geminiTool

	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			text := extractText(msg.Content)
			systemInstruction = &geminiContent{
				Parts: []geminiPart{{Text: text}},
			}
		case "user":
			contents = append(contents, geminiContent{
				Role:  "user",
				Parts: extractParts(msg),
			})
		case "assistant":
			contents = append(contents, geminiContent{
				Role:  "model",
				Parts: extractParts(msg),
			})
		case "tool":
			name := extractToolName(msg)
			contents = append(contents, geminiContent{
				Role: "function",
				Parts: []geminiPart{{
					FunctionResponse: &geminiFunctionResponse{
						Name:     name,
						Response: msg.Content,
					},
				}},
			})
		}
	}

	innerReq := geminiContentRequest{
		Contents:          contents,
		SystemInstruction: systemInstruction,
		GenerationConfig:  buildGenConfig(req),
		SessionID:         identity.DefaultVars(req.Model).SessionID,
	}

	if len(tools) > 0 {
		innerReq.Tools = tools
	}

	innerBody, _ := json.Marshal(innerReq)

	caReq := &caGenerateContentRequest{
		Model:        req.Model,
		Request:      innerBody,
		UserPromptID: uuid.New().String(),
	}

	if b.cloudAIProject != "" {
		caReq.Project = b.cloudAIProject
	}

	return caReq
}

func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var texts []string
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(raw)
}

func extractParts(msg Message) []geminiPart {
	var parts []geminiPart

	if len(msg.ToolCalls) > 0 {
		var toolCalls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		}
		if err := json.Unmarshal(msg.ToolCalls, &toolCalls); err == nil {
			for _, tc := range toolCalls {
				parts = append(parts, geminiPart{
					FunctionCall: &geminiFunctionCall{
						Name: tc.Function.Name,
						Args: tc.Function.Arguments,
					},
				})
			}
		}
	}

	text := extractText(msg.Content)
	if text != "" {
		if len(parts) == 0 {
			return []geminiPart{{Text: text}}
		}
		parts = append([]geminiPart{{Text: text}}, parts...)
	}

	if len(parts) == 0 {
		return []geminiPart{{Text: ""}}
	}
	return parts
}

func extractToolName(msg Message) string {
	var toolMsg struct {
		ToolCallID string `json:"tool_call_id"`
		Name       string `json:"name"`
	}
	if err := json.Unmarshal(msg.Content, &toolMsg); err == nil && toolMsg.Name != "" {
		return toolMsg.Name
	}
	return "unknown"
}

func buildGenConfig(req *ChatCompletionRequest) *geminiGenConfig {
	cfg := &geminiGenConfig{}
	hasConfig := false

	// Always set topK=64 — this is the default the real Gemini CLI sends for chat models.
	topK := 64
	cfg.TopK = &topK
	hasConfig = true

	if req.Temperature != nil {
		cfg.Temperature = req.Temperature
	}
	if req.MaxTokens != nil {
		cfg.MaxOutputTokens = req.MaxTokens
	}
	if req.TopP != nil && *req.TopP > 0 {
		cfg.TopP = req.TopP
	}
	if len(req.Stop) > 0 {
		cfg.StopSequences = req.Stop
	}

	if !hasConfig {
		return nil
	}
	return cfg
}

func translateFromGeminiResponse(caResp *caGenerateContentResponse, model string) (*ChatCompletionResponse, error) {
	resp := &ChatCompletionResponse{
		ID:      "chatcmpl-" + uuid.New().String()[:8],
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
	}

	if caResp.Response == nil {
		return resp, nil
	}

	choices := make([]Choice, 0)
	for i, cand := range caResp.Response.Candidates {
		choice := Choice{Index: i}
		if cand.Content != nil {
			text, toolCalls := extractResponseParts(cand.Content)
			finishReason := "stop"
			if len(toolCalls) > 0 {
				finishReason = "tool_calls"
			}
			if cand.FinishReason == "MAX_TOKENS" {
				finishReason = "length"
			}
			choice.FinishReason = &finishReason
			choice.Message = &Message{
				Role:      "assistant",
				Content:   json.RawMessage(`"` + strings.ReplaceAll(text, `"`, `\"`) + `"`),
				ToolCalls: toolCalls,
			}
		}
		choices = append(choices, choice)
	}
	resp.Choices = choices

	if caResp.Response.UsageMetadata != nil {
		resp.Usage = &Usage{
			PromptTokens:     caResp.Response.UsageMetadata.PromptTokenCount,
			CompletionTokens: caResp.Response.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      caResp.Response.UsageMetadata.TotalTokenCount,
		}
	}

	return resp, nil
}

func extractResponseParts(content *geminiContent) (string, json.RawMessage) {
	var texts []string
	var toolCalls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}

	toolCallIdx := 0
	for _, part := range content.Parts {
		if part.Text != "" && !part.Thought {
			texts = append(texts, part.Text)
		}
		if part.FunctionCall != nil {
			args := "{}"
			if len(part.FunctionCall.Args) > 0 {
				args = string(part.FunctionCall.Args)
			}
			toolCalls = append(toolCalls, struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			}{
				ID:   fmt.Sprintf("call_%s", uuid.New().String()[:8]),
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      part.FunctionCall.Name,
					Arguments: args,
				},
			})
			toolCallIdx++
		}
	}

	text := strings.Join(texts, "")
	var toolCallsJSON json.RawMessage
	if len(toolCalls) > 0 {
		tc, _ := json.Marshal(toolCalls)
		toolCallsJSON = tc
	}

	return text, toolCallsJSON
}

// --- Gemini SSE stream reader ---

type geminiStreamReader struct {
	scanner *bufio.Scanner
	respID  string
	model   string
	done    bool
	closed  bool
	buf     bytes.Buffer
}

func newGeminiStreamReader(body io.Reader, respID, model string) *geminiStreamReader {
	s := &geminiStreamReader{
		respID: respID,
		model:  model,
	}
	const maxScanToken = 4 * 1024 * 1024
	s.scanner = bufio.NewScanner(body)
	s.scanner.Buffer(make([]byte, 0, maxScanToken), maxScanToken)
	return s
}

func (r *geminiStreamReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.EOF
	}
	if r.done {
		r.closed = true
		n, err := copyBytes(p, []byte("data: [DONE]\n\n"))
		return n, err
	}

	var dataBuf strings.Builder
	for r.scanner.Scan() {
		line := r.scanner.Text()

		if line == "" {
			if dataBuf.Len() > 0 {
				break
			}
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		chunk := strings.TrimPrefix(line, "data: ")
		if dataBuf.Len() > 0 {
			dataBuf.WriteByte('\n')
		}
		dataBuf.WriteString(chunk)
	}

	if dataBuf.Len() == 0 {
		if err := r.scanner.Err(); err != nil {
			return 0, err
		}
		r.done = true
		return r.Read(p)
	}

	data := dataBuf.String()

	var caResp caGenerateContentResponse
	if err := json.Unmarshal([]byte(data), &caResp); err != nil {
		log.Debug().Err(err).Str("data", data).Msg("gemini stream: skipping unparseable chunk")
		return r.Read(p)
	}

	chunk := r.translateChunk(&caResp)
	if chunk == nil {
		return r.Read(p)
	}

	chunkJSON, err := json.Marshal(chunk)
	if err != nil {
		return r.Read(p)
	}

	sseFrame := "data: " + string(chunkJSON) + "\n\n"
	r.done = r.isFinalChunk(&caResp)
	return copyBytes(p, []byte(sseFrame))
}

func (r *geminiStreamReader) Close() error {
	r.closed = true
	return nil
}

func (r *geminiStreamReader) translateChunk(caResp *caGenerateContentResponse) *ChatCompletionResponse {
	if caResp.Response == nil {
		return nil
	}

	chunk := &ChatCompletionResponse{
		ID:      r.respID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   r.model,
	}

	for i, cand := range caResp.Response.Candidates {
		choice := Choice{Index: i}
		if cand.Content != nil && len(cand.Content.Parts) > 0 {
			var texts []string
			var toolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id,omitempty"`
				Type     string `json:"type,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function"`
			}

			for _, part := range cand.Content.Parts {
				if part.Text != "" && !part.Thought {
					texts = append(texts, part.Text)
				}
				if part.FunctionCall != nil {
					args := "{}"
					if len(part.FunctionCall.Args) > 0 {
						args = string(part.FunctionCall.Args)
					}
					tc := struct {
						Index    int    `json:"index"`
						ID       string `json:"id,omitempty"`
						Type     string `json:"type,omitempty"`
						Function struct {
							Name      string `json:"name,omitempty"`
							Arguments string `json:"arguments,omitempty"`
						} `json:"function"`
					}{
						Index: len(toolCalls),
						ID:    fmt.Sprintf("call_%s", uuid.New().String()[:8]),
						Type:  "function",
					}
					tc.Function.Name = part.FunctionCall.Name
					tc.Function.Arguments = args
					toolCalls = append(toolCalls, tc)
				}
			}

			delta := &Message{Role: "assistant"}
			if len(texts) > 0 {
				text := strings.Join(texts, "")
				delta.Content = json.RawMessage(`"` + strings.ReplaceAll(text, `"`, `\"`) + `"`)
			}
			if len(toolCalls) > 0 {
				tc, _ := json.Marshal(toolCalls)
				delta.ToolCalls = tc
			}
			choice.Delta = delta
		}

		if cand.FinishReason != "" {
			fr := mapFinishReason(cand.FinishReason)
			choice.FinishReason = &fr
		}

		chunk.Choices = append(chunk.Choices, choice)
	}

	if caResp.Response.UsageMetadata != nil {
		chunk.Usage = &Usage{
			PromptTokens:     caResp.Response.UsageMetadata.PromptTokenCount,
			CompletionTokens: caResp.Response.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      caResp.Response.UsageMetadata.TotalTokenCount,
		}
	}

	return chunk
}

func (r *geminiStreamReader) isFinalChunk(caResp *caGenerateContentResponse) bool {
	if caResp.Response == nil {
		return false
	}
	for _, cand := range caResp.Response.Candidates {
		if cand.FinishReason != "" {
			return true
		}
	}
	return false
}

func mapFinishReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY":
		return "content_filter"
	case "RECITATION":
		return "content_filter"
	default:
		return "stop"
	}
}

func copyBytes(dst, src []byte) (int, error) {
	n := copy(dst, src)
	if n < len(src) {
		return n, io.ErrShortBuffer
	}
	return n, nil
}
