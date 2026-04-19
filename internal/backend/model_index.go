package backend

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/menno/llmapiproxy/internal/config"
)

// ── Canonicalization Rules ──────────────────────────────────

// CanonicalizeRule transforms a raw model ID from a specific backend type
// into its canonical form for cross-backend matching.
type CanonicalizeRule interface {
	// Apply returns the canonical form of rawModelID.
	// backendType is the backend's config type (e.g. "ollama", "openai").
	Apply(backendType string, rawModelID string) string
}

// StripOllamaTagRule removes Ollama-specific `:tag` suffixes.
// Only applies when backendType is "ollama".
// E.g. "glm-5.1:cloud" → "glm-5.1", but "gpt-5:2025" on openai → unchanged.
type StripOllamaTagRule struct{}

func (r *StripOllamaTagRule) Apply(backendType string, rawModelID string) string {
	if backendType != "ollama" {
		return rawModelID
	}
	if idx := strings.LastIndex(rawModelID, ":"); idx > 0 {
		return rawModelID[:idx]
	}
	return rawModelID
}

// DefaultCanonicalizeRules returns the built-in set of canonicalization rules.
func DefaultCanonicalizeRules() []CanonicalizeRule {
	return []CanonicalizeRule{
		&StripOllamaTagRule{},
	}
}

// ── ModelIndex Types ────────────────────────────────────────

// BackendModelRef maps a backend to the actual model ID it uses.
type BackendModelRef struct {
	BackendName string // e.g. "zai-coding"
	BackendType string // e.g. "openai", "ollama"
	RawModelID  string // actual ID to send to this backend (e.g. "glm-5.1:cloud")
}

// IndexedModel represents a canonical model with all backends that serve it.
type IndexedModel struct {
	CanonicalID     string
	Backends        []BackendModelRef // ordered list of backends serving this model
	DisplayName     string
	OwnedBy         string // first backend that listed this model
	ContextLength   *int64
	MaxOutputTokens *int64
	Capabilities    []string
}

// ModelIndex is the single source of truth for canonical model identity and
// cross-backend overlap detection. Built from live backend data, it maps
// canonical model IDs to the set of backends that serve them.
type ModelIndex struct {
	mu      sync.RWMutex
	models  map[string]*IndexedModel // canonical ID → indexed model
	order   []string                 // sorted canonical IDs for deterministic iteration
	rules   []CanonicalizeRule
	builtAt time.Time
}

// NewModelIndex creates an empty ModelIndex with the given canonicalization rules.
func NewModelIndex(rules []CanonicalizeRule) *ModelIndex {
	return &ModelIndex{
		models: make(map[string]*IndexedModel),
		rules:  rules,
	}
}

// autoMergePrefixes scans for canonical IDs that contain "/" and checks
// if stripping the leftmost path segment(s) reveals a match with another
// canonical model. When found, the prefixed model is merged into the
// non-prefixed one.
//
// This handles backends that prepend a fixed prefix to upstream model IDs
// (e.g. "route/" from routing.run producing "route/minimax-m2.5" when the
// canonical form should be "minimax-m2.5"). The RawModelID is preserved
// so the proxy still sends the correct upstream ID.
func autoMergePrefixes(models map[string]*IndexedModel, order *[]string) {
	var prefixed []string
	for cid := range models {
		if strings.Contains(cid, "/") {
			prefixed = append(prefixed, cid)
		}
	}

	for _, cid := range prefixed {
		src, ok := models[cid]
		if !ok {
			continue
		}
		candidate := cid
		for strings.Contains(candidate, "/") {
			slash := strings.Index(candidate, "/")
			candidate = candidate[slash+1:]
			if tgt, found := models[candidate]; found {
				tgt.Backends = append(tgt.Backends, src.Backends...)
				mergeIndexedMetadata(tgt, src)
				delete(models, cid)
				for i, id := range *order {
					if id == cid {
						*order = append((*order)[:i], (*order)[i+1:]...)
						break
					}
				}
				log.Debug().
					Str("prefixed", cid).
					Str("canonical", candidate).
					Msg("model index: auto-merged prefixed model")
				break
			}
		}
	}
}

func mergeIndexedMetadata(tgt, src *IndexedModel) {
	if src.ContextLength != nil {
		if tgt.ContextLength == nil || *src.ContextLength > *tgt.ContextLength {
			tgt.ContextLength = src.ContextLength
		}
	}
	if src.MaxOutputTokens != nil {
		if tgt.MaxOutputTokens == nil || *src.MaxOutputTokens > *tgt.MaxOutputTokens {
			tgt.MaxOutputTokens = src.MaxOutputTokens
		}
	}
	if tgt.DisplayName == "" && src.DisplayName != "" {
		tgt.DisplayName = src.DisplayName
	}
	capSet := make(map[string]bool, len(tgt.Capabilities))
	for _, c := range tgt.Capabilities {
		capSet[c] = true
	}
	for _, c := range src.Capabilities {
		if !capSet[c] {
			tgt.Capabilities = append(tgt.Capabilities, c)
			capSet[c] = true
		}
	}
}

// ── Build ───────────────────────────────────────────────────

// Build populates the index from live backend data. It calls ListModels on
// each backend, canonicalizes model IDs, and groups them by canonical ID.
// backendConfigs is used to determine backend types for rule application.
// Only enabled backends are included; disabled backends are skipped entirely.
func (idx *ModelIndex) Build(ctx context.Context, backends map[string]Backend, backendConfigs []config.BackendConfig) {
	// Build type and enabled lookups from config.
	backendTypes := make(map[string]string, len(backendConfigs))
	backendEnabled := make(map[string]bool, len(backendConfigs))
	for _, bc := range backendConfigs {
		backendTypes[bc.Name] = bc.Type
		backendEnabled[bc.Name] = bc.IsEnabled()
	}

	newModels := make(map[string]*IndexedModel)
	var newOrder []string

	// Sort backend names for deterministic build order.
	sortedNames := make([]string, 0, len(backends))
	for name := range backends {
		sortedNames = append(sortedNames, name)
	}
	sort.Strings(sortedNames)

	for _, bName := range sortedNames {
		// Skip disabled backends — their models should not appear in the index.
		if !backendEnabled[bName] {
			log.Debug().Str("backend", bName).Msg("model index: skipping disabled backend")
			continue
		}

		b := backends[bName]
		bType := backendTypes[bName]

		models, err := b.ListModels(ctx)
		if err != nil {
			log.Warn().Err(err).Str("backend", bName).Msg("model index: failed to list models, skipping backend")
			continue
		}

		for _, m := range models {
			if m.Disabled {
				continue
			}

			canonicalID := idx.Canonicalize(bName, bType, m.ID)

			existing, ok := newModels[canonicalID]
			if !ok {
				existing = &IndexedModel{
					CanonicalID:     canonicalID,
					DisplayName:     m.DisplayName,
					OwnedBy:         m.OwnedBy,
					ContextLength:   m.ContextLength,
					MaxOutputTokens: m.MaxOutputTokens,
					Capabilities:    m.Capabilities,
				}
				newModels[canonicalID] = existing
				newOrder = append(newOrder, canonicalID)
			}

			// Deduplicate: skip if this backend already listed for this model
			// (handles backends that return duplicate model IDs, like copilot's gpt-4).
			alreadyListed := false
			for _, ref := range existing.Backends {
				if ref.BackendName == bName {
					alreadyListed = true
					break
				}
			}
			if !alreadyListed {
				existing.Backends = append(existing.Backends, BackendModelRef{
					BackendName: bName,
					BackendType: bType,
					RawModelID:  m.ID,
				})
			}

			// Merge metadata: max-wins for numeric fields, union for capabilities.
			idx.mergeMetadata(existing, &m)
		}
	}

	// Auto-merge models that differ only by a path prefix.
	// Handles backends that prepend a fixed prefix to upstream model IDs
	// (e.g. "route/" from routing.run producing "route/minimax-m2.5"
	// when the canonical form should be "minimax-m2.5").
	autoMergePrefixes(newModels, &newOrder)

	// Sort the order and each model's backend list.
	sort.Strings(newOrder)
	for _, im := range newModels {
		sort.Slice(im.Backends, func(i, j int) bool {
			return im.Backends[i].BackendName < im.Backends[j].BackendName
		})
	}

	idx.mu.Lock()
	idx.models = newModels
	idx.order = newOrder
	idx.builtAt = time.Now()
	idx.mu.Unlock()

	// Count overlaps for logging.
	overlaps := 0
	for _, im := range newModels {
		if len(im.Backends) > 1 {
			overlaps++
		}
	}
	log.Info().
		Int("total_models", len(newModels)).
		Int("overlapping_models", overlaps).
		Msg("model index built")
}

// mergeMetadata updates an indexed model with metadata from a new backend's model entry.
// Uses max-wins for numeric fields and union for capabilities.
func (idx *ModelIndex) mergeMetadata(existing *IndexedModel, m *Model) {
	// Max-wins for context length.
	if m.ContextLength != nil {
		if existing.ContextLength == nil || *m.ContextLength > *existing.ContextLength {
			existing.ContextLength = m.ContextLength
		}
	}
	// Max-wins for output tokens.
	if m.MaxOutputTokens != nil {
		if existing.MaxOutputTokens == nil || *m.MaxOutputTokens > *existing.MaxOutputTokens {
			existing.MaxOutputTokens = m.MaxOutputTokens
		}
	}
	// Better display name wins (non-empty over empty).
	if existing.DisplayName == "" && m.DisplayName != "" {
		existing.DisplayName = m.DisplayName
	}
	// Union capabilities.
	if len(m.Capabilities) > 0 {
		capSet := make(map[string]bool, len(existing.Capabilities))
		for _, c := range existing.Capabilities {
			capSet[c] = true
		}
		for _, c := range m.Capabilities {
			if !capSet[c] {
				existing.Capabilities = append(existing.Capabilities, c)
				capSet[c] = true
			}
		}
	}
}

// ── Query Methods ───────────────────────────────────────────

// FlatModels returns all canonical models in alphabetical order by canonical ID.
// Safe to call on a nil ModelIndex (returns nil).
func (idx *ModelIndex) FlatModels() []IndexedModel {
	if idx == nil {
		return nil
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	result := make([]IndexedModel, 0, len(idx.order))
	for _, cid := range idx.order {
		if m, ok := idx.models[cid]; ok {
			result = append(result, *m)
		}
	}
	return result
}

// Overlaps returns only models served by 2+ backends, sorted by canonical ID.
// Safe to call on a nil ModelIndex (returns nil).
func (idx *ModelIndex) Overlaps() []IndexedModel {
	if idx == nil {
		return nil
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var result []IndexedModel
	for _, cid := range idx.order {
		if m, ok := idx.models[cid]; ok && len(m.Backends) > 1 {
			result = append(result, *m)
		}
	}
	return result
}

// BackendsFor returns the backends that serve a canonical model ID.
// Safe to call on a nil ModelIndex (returns nil).
func (idx *ModelIndex) BackendsFor(canonicalID string) []BackendModelRef {
	if idx == nil {
		return nil
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if m, ok := idx.models[canonicalID]; ok {
		refs := make([]BackendModelRef, len(m.Backends))
		copy(refs, m.Backends)
		return refs
	}
	return nil
}

// ResolveBackendModelID returns the actual model ID to send to a specific
// backend for a given canonical model ID. Returns ("", false) if the
// backend doesn't serve this model.
// Safe to call on a nil ModelIndex.
func (idx *ModelIndex) ResolveBackendModelID(canonicalID, backendName string) (string, bool) {
	if idx == nil {
		return "", false
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	m, ok := idx.models[canonicalID]
	if !ok {
		return "", false
	}
	for _, ref := range m.Backends {
		if ref.BackendName == backendName {
			return ref.RawModelID, true
		}
	}
	return "", false
}

// Lookup returns the IndexedModel for a canonical ID, or nil if not found.
// Safe to call on a nil ModelIndex.
func (idx *ModelIndex) Lookup(canonicalID string) *IndexedModel {
	if idx == nil {
		return nil
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if m, ok := idx.models[canonicalID]; ok {
		cp := *m
		return &cp
	}
	return nil
}

// Canonicalize applies the canonicalization rules to a raw model ID from
// a given backend. It strips the backend name prefix first, then applies
// each rule in sequence.
func (idx *ModelIndex) Canonicalize(backendName, backendType, rawModelID string) string {
	// Strip the backend name prefix (e.g. "openrouter/gpt-5" → "gpt-5").
	cid := rawModelID
	if strings.HasPrefix(cid, backendName+"/") {
		cid = cid[len(backendName)+1:]
	}

	// Apply each canonicalization rule.
	for _, rule := range idx.rules {
		cid = rule.Apply(backendType, cid)
	}

	return cid
}

// Age returns the time since the index was last built. Returns 0 if never built.
func (idx *ModelIndex) Age() time.Duration {
	if idx == nil {
		return 0
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.builtAt.IsZero() {
		return 0
	}
	return time.Since(idx.builtAt)
}

// BuiltAt returns when the index was last built. Returns zero time if never built.
func (idx *ModelIndex) BuiltAt() time.Time {
	if idx == nil {
		return time.Time{}
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.builtAt
}

// Len returns the number of canonical models in the index.
// Safe to call on a nil ModelIndex (returns 0).
func (idx *ModelIndex) Len() int {
	if idx == nil {
		return 0
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.models)
}
