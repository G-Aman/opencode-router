package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type ModelsDevMeta struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Family      string                 `json:"family"`
	Reasoning   bool                   `json:"reasoning"`
	ToolCall    bool                   `json:"tool_call"`
	Modalities  map[string][]string    `json:"modalities"`
	Limit       map[string]int         `json:"limit"`
	Cost        map[string]interface{} `json:"cost"`
}

var (
	metaCacheMu sync.RWMutex
	metaCache   = make(map[string]ModelsDevMeta)
	metaLastAt  int64
)

func metaCacheFilePath() string {
	dir := filepath.Dir(configPath)
	return filepath.Join(dir, "models-dev-cache.json")
}

func loadPersistedMeta() {
	b, err := os.ReadFile(metaCacheFilePath())
	if err != nil {
		return
	}
	var saved map[string]ModelsDevMeta
	if err := json.Unmarshal(b, &saved); err == nil && len(saved) > 0 {
		metaCacheMu.Lock()
		metaCache = saved
		metaCacheMu.Unlock()
		addLog(fmt.Sprintf("loaded %d model metadata specs from disk cache", len(saved)))
	}
}

func savePersistedMeta() {
	metaCacheMu.RLock()
	copyMap := make(map[string]ModelsDevMeta, len(metaCache))
	for k, v := range metaCache {
		copyMap[k] = v
	}
	metaCacheMu.RUnlock()

	b, err := json.MarshalIndent(copyMap, "", "  ")
	if err == nil {
		os.WriteFile(metaCacheFilePath(), b, 0644)
	}
}

// syncModelsDevMetadata pulls latest OpenCode metadata from models.dev with 30s timeout
func syncModelsDevMetadata(force bool) {
	now := time.Now().Unix()
	metaCacheMu.RLock()
	last := metaLastAt
	count := len(metaCache)
	metaCacheMu.RUnlock()

	cfgMu.RLock()
	endpoint := cfg.MetaEndpoint
	if endpoint == "" {
		endpoint = "https://models.dev/api.json"
	}
	timeoutSec := cfg.MetaTimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	syncHours := cfg.MetaSyncHours
	if syncHours <= 0 {
		syncHours = 24
	}
	cfgMu.RUnlock()

	intervalSec := int64(syncHours * 3600)
	if !force && count > 0 && (now-last < intervalSec) {
		return
	}

	client := &http.Client{Timeout: time.Duration(timeoutSec) * time.Second}
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "opencode-router/1.0 (models-sync)")

	resp, err := client.Do(req)
	if err != nil {
		addLog(fmt.Sprintf("models.dev metadata sync failed (retaining existing): %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		addLog(fmt.Sprintf("models.dev metadata returned HTTP %d (retaining existing)", resp.StatusCode))
		return
	}

	dec := json.NewDecoder(resp.Body)
	// Expect start of JSON object "{"
	t, err := dec.Token()
	if err != nil || t != json.Delim('{') {
		addLog(fmt.Sprintf("models.dev unexpected root token: %v", err))
		return
	}

	extracted := make(map[string]ModelsDevMeta)
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			break
		}
		key, ok := t.(string)
		if !ok {
			break
		}

		if key == "opencode" || key == "opencode-go" {
			var prov struct {
				Models map[string]ModelsDevMeta `json:"models"`
			}
			if err := dec.Decode(&prov); err == nil && prov.Models != nil {
				for mid, meta := range prov.Models {
					extracted[mid] = meta
				}
			}
		} else {
			// Discard other 220+ providers token-by-token without allocating RAM
			var discard json.RawMessage
			_ = dec.Decode(&discard)
		}
	}

	if len(extracted) > 0 {
		metaCacheMu.Lock()
		for k, v := range extracted {
			metaCache[k] = v
		}
		metaLastAt = now
		metaCacheMu.Unlock()
		savePersistedMeta()
		addLog(fmt.Sprintf("models.dev sync ok: %d models updated", len(extracted)))
		invalidateModelsCache()
	}
}

func getDynamicModelMeta(id string) (map[string]interface{}, bool) {
	metaCacheMu.RLock()
	defer metaCacheMu.RUnlock()
	m, ok := metaCache[id]
	if !ok {
		return nil, false
	}

	ctx := 128000
	if c, has := m.Limit["context"]; has && c > 0 {
		ctx = c
	}
	maxTokens := 32000
	if out, has := m.Limit["output"]; has && out > 0 {
		maxTokens = out
	}

	modalities := []string{"text"}
	if inp, has := m.Modalities["input"]; has && len(inp) > 0 {
		modalities = inp
	}

	pricing := map[string]interface{}{"prompt": "0", "completion": "0"}
	if m.Cost != nil {
		pricing = m.Cost
	}

	return map[string]interface{}{
		"id":                 id,
		"name":               m.Name,
		"description":        m.Description,
		"object":             "model",
		"created":            time.Now().Unix(),
		"owned_by":           "opencode",
		"context_window":     ctx,
		"context_length":     ctx,
		"max_tokens":         maxTokens,
		"max_output_tokens":  maxTokens,
		"modalities":         modalities,
		"supports_tools":     m.ToolCall,
		"supports_reasoning": m.Reasoning,
		"pricing":            pricing,
		"top_provider": map[string]interface{}{
			"context_length":        ctx,
			"max_completion_tokens": maxTokens,
		},
		"per_request_limits": map[string]interface{}{
			"context": ctx,
			"output":  maxTokens,
		},
	}, true
}
