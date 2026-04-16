package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// OllamaManager defines model management operations specific to Ollama.
// All methods communicate via Ollama's native API (/api/pull, /api/delete,
// /api/show, /api/ps) regardless of which compat_mode the backend uses.
type OllamaManager interface {
	// PullModel starts pulling a model. Returns a channel of progress updates.
	PullModel(ctx context.Context, modelName string) (<-chan OllamaPullProgress, error)

	// DeleteModel removes a model from the Ollama instance.
	DeleteModel(ctx context.Context, modelName string) error

	// ShowModelDetails returns detailed information about a model.
	ShowModelDetails(ctx context.Context, modelName string) (*OllamaModelInfo, error)

	// ListRunningModels returns models currently loaded in memory.
	ListRunningModels(ctx context.Context) ([]OllamaRunningModel, error)

	// ActivePulls returns currently active pull operations.
	ActivePulls() []OllamaPullStatus

	// Whoami checks the signin status with ollama.com via the local Ollama server.
	// Returns the username if signed in, or a signin URL if not.
	Whoami(ctx context.Context) (*OllamaWhoamiResponse, error)

	// Signout disconnects the device from ollama.com.
	Signout(ctx context.Context) error
}

// OllamaPullProgress represents a single progress update during model pull.
type OllamaPullProgress struct {
	Status     string `json:"status"`
	Digest     string `json:"digest,omitempty"`
	Total      int64  `json:"total,omitempty"`
	Completed  int64  `json:"completed,omitempty"`
	Percentage int    `json:"percentage,omitempty"`
	Error      string `json:"error,omitempty"`
	Done       bool   `json:"done"`
}

// OllamaPullStatus is a snapshot of an active or completed pull operation.
type OllamaPullStatus struct {
	ModelName string             `json:"model_name"`
	PullID    string             `json:"pull_id"`
	StartedAt time.Time          `json:"started_at"`
	Progress  OllamaPullProgress `json:"progress"`
}

// OllamaModelInfo contains detailed model information from /api/show.
type OllamaModelInfo struct {
	License      string                `json:"license,omitempty"`
	ModifiedAt   string                `json:"modified_at,omitempty"`
	Template     string                `json:"template,omitempty"`
	Parameters   string                `json:"parameters,omitempty"`
	Details      OllamaModelDetailsExt `json:"details"`
	Capabilities []string              `json:"capabilities,omitempty"`
	ModelInfo    map[string]any        `json:"model_info,omitempty"`
}

// OllamaModelDetailsExt is the extended model details from /api/show.
type OllamaModelDetailsExt struct {
	ParentModel       string   `json:"parent_model"`
	Format            string   `json:"format"`
	Family            string   `json:"family"`
	Families          []string `json:"families"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
}

// OllamaRunningModel is a model currently loaded in memory from /api/ps.
type OllamaRunningModel struct {
	Name          string             `json:"name"`
	Model         string             `json:"model"`
	Size          int64              `json:"size"`
	Digest        string             `json:"digest"`
	Details       ollamaModelDetails `json:"details"`
	ExpiresAt     string             `json:"expires_at,omitempty"`
	SizeVRAM      int64              `json:"size_vram,omitempty"`
	ContextLength int                `json:"context_length,omitempty"`
}

// ollamaManagerImpl implements OllamaManager for an OllamaBackend.
// Pull state is stored on the OllamaBackend itself so it survives page reloads
// and manager re-creation.
type ollamaManagerImpl struct {
	backend *OllamaBackend
}

// NewOllamaManager creates a new OllamaManager for the given OllamaBackend.
func NewOllamaManager(b *OllamaBackend) OllamaManager {
	return &ollamaManagerImpl{backend: b}
}

// PullModel streams a model pull operation. It reads the NDJSON response from
// Ollama's /api/pull endpoint and sends progress updates to the returned channel.
func (m *ollamaManagerImpl) PullModel(ctx context.Context, modelName string) (<-chan OllamaPullProgress, error) {
	reqBody := map[string]any{
		"model":  modelName,
		"stream": true,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling pull request: %w", err)
	}

	pullID := fmt.Sprintf("%s-%d", modelName, time.Now().UnixNano())
	status := &OllamaPullStatus{
		ModelName: modelName,
		PullID:    pullID,
		StartedAt: time.Now(),
	}
	m.backend.StorePull(pullID, status)

	// Use a detached context so the pull survives browser disconnects.
	// Model pulls can take minutes; the HTTP request context would be
	// canceled as soon as the browser XHR connection drops.
	pullCtx, pullCancel := context.WithCancel(context.Background())
	m.backend.StoreCancel(pullID, pullCancel)

	httpReq, err := http.NewRequestWithContext(pullCtx, http.MethodPost, m.backend.baseURL+"/api/pull", strings.NewReader(string(body)))
	if err != nil {
		pullCancel()
		m.backend.DeletePull(pullID)
		return nil, fmt.Errorf("creating pull request: %w", err)
	}
	m.backend.setHeaders(httpReq, "")

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		pullCancel()
		m.backend.DeletePull(pullID)
		return nil, fmt.Errorf("sending pull request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		pullCancel()
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		m.backend.DeletePull(pullID)
		return nil, fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, string(errBody))
	}

	ch := make(chan OllamaPullProgress, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		defer pullCancel()
		defer func() {
			// Keep completed pulls for a while so status can be queried.
			time.AfterFunc(5*time.Minute, func() {
				m.backend.DeletePull(pullID)
			})
		}()

		decoder := json.NewDecoder(resp.Body)
		for {
			var progress OllamaPullProgress
			if err := decoder.Decode(&progress); err != nil {
				if err != io.EOF {
					log.Error().Err(err).Str("model", modelName).Msg("ollama pull stream error")
					ch <- OllamaPullProgress{Status: "error", Error: err.Error(), Done: true}
				} else {
					// Stream ended without explicit "success" — treat as done.
					progress = OllamaPullProgress{Status: "success", Done: true}
					ch <- progress
				}
				break
			}

			if progress.Total > 0 && progress.Completed > 0 {
				progress.Percentage = int(float64(progress.Completed) / float64(progress.Total) * 100)
			}

			m.backend.UpdatePullProgress(pullID, progress)

			ch <- progress

			if progress.Status == "success" || progress.Done {
				break
			}
		}

		// Clear model cache after successful pull.
		m.backend.ClearModelCache()
	}()

	return ch, nil
}

// DeleteModel removes a model from the Ollama instance.
func (m *ollamaManagerImpl) DeleteModel(ctx context.Context, modelName string) error {
	reqBody := map[string]string{"model": modelName}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshaling delete request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, m.backend.baseURL+"/api/delete", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("creating delete request: %w", err)
	}
	m.backend.setHeaders(httpReq, "")

	resp, err := m.backend.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sending delete request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, string(errBody))
	}

	// Clear model cache after delete.
	m.backend.ClearModelCache()
	return nil
}

// ShowModelDetails fetches detailed information about a model.
func (m *ollamaManagerImpl) ShowModelDetails(ctx context.Context, modelName string) (*OllamaModelInfo, error) {
	reqBody := map[string]any{"model": modelName, "verbose": false}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling show request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.backend.baseURL+"/api/show", strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("creating show request: %w", err)
	}
	m.backend.setHeaders(httpReq, "")

	resp, err := m.backend.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending show request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, string(errBody))
	}

	var info OllamaModelInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decoding show response: %w", err)
	}
	return &info, nil
}

// ListRunningModels returns models currently loaded in memory.
func (m *ollamaManagerImpl) ListRunningModels(ctx context.Context) ([]OllamaRunningModel, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, m.backend.baseURL+"/api/ps", nil)
	if err != nil {
		return nil, fmt.Errorf("creating ps request: %w", err)
	}
	m.backend.setHeaders(httpReq, "")

	resp, err := m.backend.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending ps request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, string(errBody))
	}

	var result struct {
		Models []OllamaRunningModel `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding ps response: %w", err)
	}
	return result.Models, nil
}

// OllamaWhoamiResponse contains the signin status from the Ollama server.
type OllamaWhoamiResponse struct {
	Name      string `json:"name,omitempty"`
	SigninURL string `json:"signin_url,omitempty"`
}

// ActivePulls returns snapshots of all active or recently completed pulls.
func (m *ollamaManagerImpl) ActivePulls() []OllamaPullStatus {
	return m.backend.ActivePulls()
}

// Whoami checks the signin status via the local Ollama server's /api/me endpoint.
// The local server forwards the request to ollama.com.
// Returns username if signed in, or a signin URL if not.
func (m *ollamaManagerImpl) Whoami(ctx context.Context) (*OllamaWhoamiResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.backend.baseURL+"/api/me", nil)
	if err != nil {
		return nil, fmt.Errorf("creating whoami request: %w", err)
	}
	m.backend.setHeaders(httpReq, "")

	resp, err := m.backend.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending whoami request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading whoami response: %w", err)
	}

	// 200 = signed in, response has {"name":"username"}
	if resp.StatusCode == http.StatusOK {
		var result struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("decoding whoami response: %w", err)
		}
		return &OllamaWhoamiResponse{Name: result.Name}, nil
	}

	// 401 = not signed in, response has {"error":"unauthorized","signin_url":"..."}
	if resp.StatusCode == http.StatusUnauthorized {
		var result struct {
			SigninURL string `json:"signin_url"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("decoding whoami 401 response: %w", err)
		}
		return &OllamaWhoamiResponse{SigninURL: result.SigninURL}, nil
	}

	return nil, fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, string(body))
}

// Signout disconnects the device from ollama.com via the local Ollama server.
func (m *ollamaManagerImpl) Signout(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.backend.baseURL+"/api/signout", nil)
	if err != nil {
		return fmt.Errorf("creating signout request: %w", err)
	}
	m.backend.setHeaders(httpReq, "")

	resp, err := m.backend.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sending signout request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, string(errBody))
	}
	return nil
}
