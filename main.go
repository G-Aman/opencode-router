package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"net/url"
)

func modelFormat(id string) string {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	for _, p := range cfg.ResponsesModels {
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "*") {
			prefix := strings.TrimSuffix(p, "*")
			if strings.HasPrefix(id, prefix) {
				return "responses"
			}
		} else if id == p {
			return "responses"
		}
	}
	return "chat"
}

func resolveModelName(rawModel string) string {
	parts := strings.Split(rawModel, "/")
	model := parts[len(parts)-1]
	if model == "" {
		model = "nemotron-3-ultra-free"
	}
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	seenAliases := make(map[string]bool)
	for hops := 0; hops < 5; hops++ {
		if alias, ok := cfg.ModelAliases[model]; ok && alias != "" && !seenAliases[alias] {
			seenAliases[model] = true
			model = alias
		} else {
			break
		}
	}
	return model
}




type Config struct {
	Host              string            `json:"host"`
	Port              int               `json:"port"`
	Upstream          string            `json:"upstream"`
	UA                string            `json:"ua"`
	AutoUA            bool              `json:"autoUA"`
	InjectSession     bool              `json:"injectSession"`
	DefaultModel      string            `json:"defaultModel"`
	FallbackModels    []string          `json:"fallbackModels"`
	DisabledModels    []string          `json:"disabledModels"`
	ModelAliases      map[string]string `json:"modelAliases"`
	ResponsesModels   []string          `json:"responsesModels"`
	RateLimitMax      int               `json:"rateLimitMax"`
	RateLimitWindowMs int               `json:"rateLimitWindowMs"`
	ProxyKey          string            `json:"proxyKey"`
	DefaultUpstreamKey string            `json:"defaultUpstreamKey"`
	OutboundProxy     string            `json:"outboundProxy"`
	TrustForwarded    bool              `json:"trustForwarded"`
	TimeoutMs         int               `json:"timeoutMs"`
	CacheMs           int               `json:"cacheMs"`
	AutoSync          bool              `json:"autoSync"`
	AutoSyncIntervalMs int               `json:"autoSyncIntervalMs"`
	UaRefreshDays      float64           `json:"uaRefreshDays"`
	UaRefreshMs        int               `json:"uaRefreshMs"`
	MetaEndpoint       string            `json:"metaEndpoint"`
	MetaTimeoutSec     int               `json:"metaTimeoutSec"`
	MetaSyncHours      int               `json:"metaSyncHours"`
}

var (
	cfgMu         sync.RWMutex
	cfg           Config
	configPath    string
	baseToolsChat []map[string]interface{}
	baseToolsMuse []map[string]interface{}
	sysPromptChat string
	sysPromptMuse string
	htmlIndex     []byte
	assetsDir     string
	startTime     = time.Now()

	// Stats
	statsMu       sync.Mutex
	totalReqs     int64
	totalErrors   int64
	window60      []time.Time
	perMinute     = make(map[int64]int64)
	recentHistory []map[string]interface{}

	// Logs
	logMu    sync.Mutex
	logLines []map[string]string
)

// getUpstreamClient returns an http.Client configured with timeouts and optional outbound proxy
func getUpstreamClient(timeout time.Duration) *http.Client {
	cfgMu.RLock()
	proxyStr := strings.TrimSpace(cfg.OutboundProxy)
	cfgMu.RUnlock()

	client := &http.Client{Timeout: timeout}
	if proxyStr != "" {
		if proxyURL, err := url.Parse(proxyStr); err == nil {
			client.Transport = &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			}
		} else {
			log.Printf("proxy: invalid outbound proxy URL %q: %v", proxyStr, err)
		}
	}
	return client
}

func addLog(msg string) {
	logMu.Lock()
	defer logMu.Unlock()
	entry := map[string]string{
		"at":  time.Now().Format("Jan 02, 15:04:05"),
		"msg": msg,
	}
	logLines = append(logLines, entry)
	if len(logLines) > 500 {
		logLines = logLines[1:]
	}
	log.Println(msg)
}

func recordReq(status int, model string, ms int64) {
	recordObserved(model, status < 400)
	statsMu.Lock()
	defer statsMu.Unlock()
	totalReqs++
	if status >= 400 {
		totalErrors++
	}
	now := time.Now()
	window60 = append(window60, now)
	min := now.Unix() / 60
	perMinute[min]++
	for m := range perMinute {
		if min-m > 10 {
			delete(perMinute, m)
		}
	}

	entry := map[string]interface{}{
		"model":  model,
		"status": status,
		"ms":     ms,
		"time":   now.Format("Jan 02, 15:04:05"),
	}
	recentHistory = append([]map[string]interface{}{entry}, recentHistory...)
	if len(recentHistory) > 50 {
		recentHistory = recentHistory[:50]
	}
}

func genSessionID() string {
	nowMs := time.Now().UnixNano() / 1e6
	invPart := uint32(0xFFFFFFFF - (nowMs & 0xFFFFFFFF))
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	randPart := make([]byte, 14)
	for i := range randPart {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		randPart[i] = chars[n.Int64()]
	}
	return fmt.Sprintf("ses_f%08xffe%s", invPart, string(randPart))
}

func genRequestID() string {
	nowMs := time.Now().UnixNano() / 1e6
	tPart := fmt.Sprintf("%09x", nowMs&0xFFFFFFFFF)
	if len(tPart) > 8 {
		tPart = tPart[len(tPart)-8:]
	}
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	randPart := make([]byte, 14)
	for i := range randPart {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		randPart[i] = chars[n.Int64()]
	}
	return fmt.Sprintf("msg_0%s001%s", tPart, string(randPart))
}

func loadResources(dir string) error {
	configPath = filepath.Join(dir, "opencode-router.json")
	if b, err := os.ReadFile(configPath); err == nil {
		json.Unmarshal(b, &cfg)
	} else {
		cfg = Config{
			Host:              "0.0.0.0",
			Port:              8787,
			Upstream:          "https://opencode.ai/zen/v1",
			UA:                "opencode/1.18.32 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14",
			AutoUA:            true,
			InjectSession:     true,
			TimeoutMs:         120000,
			RateLimitWindowMs: 60000,
			ProxyKey:          "",
			MetaEndpoint:      "https://models.dev/api.json",
			MetaTimeoutSec:    30,
			MetaSyncHours:     24,
			DefaultModel:      "nemotron-3.5-lightning-free",
			FallbackModels: []string{
				"ling-3.0-flash-fin-free",
				"nemotron-3-ultra-free",
				"muse-spark-1.3-contributor-free",
				"muse-spark-1.2-contributor-free",
				"mimo-v2.6-flash-free",
				"longcat-2.5-preview-free",
				"nemotron-3.5-lightning-free",
				"mimo-v2.5-free",
				"space-bunny-free",
			},
			ModelAliases:    make(map[string]string),
			ResponsesModels: []string{"gpt-5*", "gpt-6*", "grok-*", "muse-spark-*"},
		}
		if b, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			os.WriteFile(configPath, b, 0644)
		}
	}

	if b, err := os.ReadFile(filepath.Join(dir, "base_tools.json")); err == nil {
		json.Unmarshal(b, &baseToolsChat)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "base_tools_muse.json")); err == nil {
		json.Unmarshal(b, &baseToolsMuse)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "system_prompt_chat.txt")); err == nil {
		sysPromptChat = string(b)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "system_prompt_muse.txt")); err == nil {
		sysPromptMuse = string(b)
	}

	htmlPath := filepath.Join(dir, "public", "index.html")
	if b, err := os.ReadFile(htmlPath); err == nil {
		htmlIndex = b
	}

	assetsDir = filepath.Join(dir, "assets")
	return nil
}

type ModelInfo struct {
	ID              string                 `json:"id"`
	Object          string                 `json:"object"`
	Created         int64                  `json:"created"`
	OwnedBy         string                 `json:"owned_by"`
	ContextWindow   int                    `json:"context_window,omitempty"`
	ContextLength   int                    `json:"context_length,omitempty"`
	MaxTokens       int                    `json:"max_tokens,omitempty"`
	MaxOutputTokens int                    `json:"max_output_tokens,omitempty"`
	Modalities      []string               `json:"modalities,omitempty"`
	SupportsTools   bool                   `json:"supports_tools"`
	SupportsReason  bool                   `json:"supports_reasoning"`
	Pricing         map[string]interface{} `json:"pricing"`
	Architecture    map[string]interface{} `json:"architecture,omitempty"`
	TopProvider     map[string]interface{} `json:"top_provider,omitempty"`
	PerReqLimits    map[string]interface{} `json:"per_request_limits,omitempty"`
}

var modelCatalog = []ModelInfo{
	{
		ID:              "muse-spark-1.3-contributor-free",
		Object:          "model",
		Created:         1740000000,
		OwnedBy:         "opencode",
		ContextWindow:   1048576,
		ContextLength:   1048576,
		MaxTokens:       131072,
		MaxOutputTokens: 131072,
		Modalities:      []string{"text", "image"},
		SupportsTools:   true,
		SupportsReason:  true,
		Pricing:         map[string]interface{}{"prompt": "0", "completion": "0"},
		Architecture:    map[string]interface{}{"modality": "text->text"},
		TopProvider:     map[string]interface{}{"context_length": 1048576, "max_completion_tokens": 131072},
		PerReqLimits:    map[string]interface{}{"context": 1048576, "output": 131072},
	},
	{
		ID:              "nemotron-3-ultra-free",
		Object:          "model",
		Created:         1740000000,
		OwnedBy:         "opencode",
		ContextWindow:   1000000,
		ContextLength:   1000000,
		MaxTokens:       128000,
		MaxOutputTokens: 128000,
		Modalities:      []string{"text"},
		SupportsTools:   true,
		SupportsReason:  true,
		Pricing:         map[string]interface{}{"prompt": "0", "completion": "0"},
		Architecture:    map[string]interface{}{"modality": "text->text"},
		TopProvider:     map[string]interface{}{"context_length": 1000000, "max_completion_tokens": 128000},
		PerReqLimits:    map[string]interface{}{"context": 1000000, "output": 128000},
	},
	{
		ID:              "mimo-v2.6-flash-free",
		Object:          "model",
		Created:         1740000000,
		OwnedBy:         "opencode",
		ContextWindow:   200000,
		ContextLength:   200000,
		MaxTokens:       32000,
		MaxOutputTokens: 32000,
		Modalities:      []string{"text"},
		SupportsTools:   true,
		SupportsReason:  true,
		Pricing:         map[string]interface{}{"prompt": "0", "completion": "0"},
		Architecture:    map[string]interface{}{"modality": "text->text"},
		TopProvider:     map[string]interface{}{"context_length": 200000, "max_completion_tokens": 32000},
		PerReqLimits:    map[string]interface{}{"context": 200000, "output": 32000},
	},
	{
		ID:              "space-bunny-free",
		Object:          "model",
		Created:         1740000000,
		OwnedBy:         "opencode",
		ContextWindow:   1048576,
		ContextLength:   1048576,
		MaxTokens:       524288,
		MaxOutputTokens: 524288,
		Modalities:      []string{"text", "image"},
		SupportsTools:   true,
		SupportsReason:  true,
		Pricing:         map[string]interface{}{"prompt": "0", "completion": "0"},
		Architecture:    map[string]interface{}{"modality": "text->text"},
		TopProvider:     map[string]interface{}{"context_length": 1048576, "max_completion_tokens": 524288},
		PerReqLimits:    map[string]interface{}{"context": 1048576, "output": 524288},
	},
	{
		ID:              "ling-3.0-flash-fin-free",
		Object:          "model",
		Created:         1740000000,
		OwnedBy:         "opencode",
		ContextWindow:   256000,
		ContextLength:   256000,
		MaxTokens:       32000,
		MaxOutputTokens: 32000,
		Modalities:      []string{"text"},
		SupportsTools:   true,
		SupportsReason:  true,
		Pricing:         map[string]interface{}{"prompt": "0", "completion": "0"},
		Architecture:    map[string]interface{}{"modality": "text->text"},
		TopProvider:     map[string]interface{}{"context_length": 256000, "max_completion_tokens": 32000},
		PerReqLimits:    map[string]interface{}{"context": 256000, "output": 32000},
	},
}

func extractContentText(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	if arr, ok := v.([]interface{}); ok {
		var parts []string
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if limited, retryAfter := rateLimitFor(r); limited {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"type":    "rate_limit_error",
				"message": "Too many requests. Please slow down.",
			},
		})
		return
	}
	cfgMu.RLock()
	proxyKey := cfg.ProxyKey
	cfgMu.RUnlock()

	if proxyKey != "" {
		inAuth := r.Header.Get("Authorization")
		token := strings.TrimSpace(strings.TrimPrefix(inAuth, "Bearer "))
		apiKey := strings.TrimSpace(r.Header.Get("x-api-key"))
		if token == "" && apiKey != "" {
			token = apiKey
		}
		upstreamHeader := strings.TrimSpace(r.Header.Get("x-zen-key"))

		isByok := strings.HasPrefix(token, "zen_") || strings.HasPrefix(token, "oc_") ||
			strings.HasPrefix(upstreamHeader, "zen_") || strings.HasPrefix(upstreamHeader, "oc_")

		if subtle.ConstantTimeCompare([]byte(token), []byte(proxyKey)) != 1 && !isByok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"invalid proxy key"}}`))
			return
		}
	}
	data, ok := fetchModels()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   data,
		"ok":     ok,
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	cfgMu.RLock()
	up := cfg.Upstream
	cfgMu.RUnlock()
	_, ok := fetchModels()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":       ok,
		"upstream": up,
	})
}

func adminAuth(w http.ResponseWriter, r *http.Request) bool {
	cfgMu.RLock()
	proxyKey := cfg.ProxyKey
	cfgMu.RUnlock()
	if proxyKey == "" {
		return true
	}
	inAuth := r.Header.Get("Authorization")
	token := strings.TrimPrefix(inAuth, "Bearer ")
	apiKey := r.Header.Get("x-api-key")
	if token == "" && apiKey != "" {
		token = apiKey
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(proxyKey)) != 1 {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return false
	}
	return true
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if !adminAuth(w, r) {
		return
	}
	statsMu.Lock()
	now := time.Now()
	cutoff := now.Add(-60 * time.Second)
	validIdx := 0
	for i, t := range window60 {
		if t.After(cutoff) {
			validIdx = i
			break
		}
	}
	window60 = window60[validIdx:]
	lastMinute := len(window60)

	curMin := now.Unix() / 60
	var last5m int64
	for m, c := range perMinute {
		if curMin-m <= 5 {
			last5m += c
		}
	}
	recCopy := make([]map[string]interface{}, len(recentHistory))
	copy(recCopy, recentHistory)
	totReq := totalReqs
	totErr := totalErrors
	statsMu.Unlock()

	cfgMu.RLock()
	defer cfgMu.RUnlock()

	liveModels, modelsOk := fetchModels()
	servedModels := make([]string, len(liveModels))
	for i, m := range liveModels {
		if id, ok := m["id"].(string); ok {
			servedModels[i] = id
		}
	}

	syncStateMu.Lock()
	syncCopy := map[string]interface{}{
		"ok":          syncState.ok,
		"at":          syncState.at,
		"running":     syncState.running,
		"ms":          syncState.ms,
		"working":     emptyIfNil(syncState.working),
		"rateLimited": emptyIfNil(syncState.rateLimited),
		"gated":       emptyIfNil(syncState.gated),
		"flaky":       emptyIfNil(syncState.flaky),
		"dead":        emptyIfNil(syncState.dead),
		"error":       syncState.error,
		"observed":    getObservedCopy(),
	}
	syncStateMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"uptime":     int(time.Since(startTime).Seconds()),
		"upstreamOk": modelsOk,
		"upstream":      cfg.Upstream,
		"ua":            cfg.UA,
		"uaAutoVersion": uaAutoState.version,
		"auth":       map[string]interface{}{"mode": "proxy", "proxyKey": cfg.ProxyKey != ""},
		"requests": map[string]interface{}{
			"total":      totReq,
			"errors":     totErr,
			"lastMinute": lastMinute,
			"last5m":     last5m,
		},
		"recent": recCopy,
		"models": map[string]interface{}{
			"total":      len(liveModels),
			"allowed":    len(servedModels),
			"served":     servedModels,
			"discovered": getUpstreamDiscovered(),
		},
		"sync": syncCopy,
	})
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	if !adminAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	n := 200
	if nStr := r.URL.Query().Get("n"); nStr != "" {
		if val, err := strconv.Atoi(nStr); err == nil && val > 0 {
			n = val
		}
	}

	logMu.Lock()
	total := len(logLines)
	start := 0
	if total > n {
		start = total - n
	}
	copied := make([]map[string]string, len(logLines[start:]))
	copy(copied, logLines[start:])
	logMu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"logs": copied,
	})
}

func handleApiConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.WriteHeader(http.StatusOK)
		return
	}
	if !adminAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")

	if r.Method == "GET" {
		cfgMu.RLock()
		defer cfgMu.RUnlock()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"config": cfg,
		})
		return
	}

	if r.Method == "PUT" {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":"failed to read body"}`, http.StatusBadRequest)
			return
		}

		var newCfg Config
		if err := json.Unmarshal(bodyBytes, &newCfg); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}

		cfgMu.Lock()
		// Preserve sensitive/internal fields if omitted or masked
		var checkMap map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &checkMap); err == nil {
			if _, ok := checkMap["defaultUpstreamKey"]; !ok {
				newCfg.DefaultUpstreamKey = cfg.DefaultUpstreamKey
			}
		}
		if newCfg.ProxyKey == "••••••••" {
			newCfg.ProxyKey = cfg.ProxyKey
		}

		cfg = newCfg
		if b, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			os.WriteFile(configPath, b, 0644)
		}
		resCfg := cfg
		cfgMu.Unlock()
		invalidateModelsCache()
		if cfg.AutoUA {
			go refreshUA(true)
		}
		addLog("config updated via UI")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":     true,
			"config": resCfg,
		})
		return
	}

	if r.Method == "POST" {
		invalidateModelsCache()
		addLog("config model list restored to defaults")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":     true,
			"config": cfg,
		})
		return
	}

	http.NotFound(w, r)
}

func handleApiTest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if limited, retryAfter := rateLimitFor(r); limited {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"type":    "rate_limit_error",
				"message": "Too many requests. Please slow down.",
			},
		})
		return
	}

	var reqBody map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		http.Error(w, `{"ok":false,"error":"invalid body"}`, http.StatusBadRequest)
		return
	}

	model, _ := reqBody["model"].(string)
	if model == "" {
		model = "nemotron-3-ultra-free"
	}
	cfgMu.RLock()
	seenAliases := make(map[string]bool)
	for hops := 0; hops < 5; hops++ {
		if alias, ok := cfg.ModelAliases[model]; ok && alias != "" && !seenAliases[alias] {
			seenAliases[model] = true
			model = alias
		} else {
			break
		}
	}
	cfgMu.RUnlock()
	start := time.Now()

	// Perform actual test probe
	sessionID := genSessionID()
	requestID := genRequestID()
	cfgMu.RLock()
	testAuth := "Bearer public"
	if upstreamKey, ok := reqBody["upstreamKey"].(string); ok && strings.TrimSpace(upstreamKey) != "" {
		testAuth = "Bearer " + strings.TrimSpace(upstreamKey)
	} else if cfg.DefaultUpstreamKey != "" {
		testAuth = "Bearer " + cfg.DefaultUpstreamKey
	}
	testUA := cfg.UA
	upstreamBase := cfg.Upstream
	cfgMu.RUnlock()

	headers := http.Header{
		"Authorization":     []string{testAuth},
		"Content-Type":      []string{"application/json"},
		"User-Agent":        []string{testUA},
		"x-opencode-client":  []string{"cli"},
		"x-opencode-project": []string{"global"},
		"x-opencode-session": []string{sessionID},
		"x-opencode-request": []string{requestID},
		"Accept":            []string{"*/*"},
	}

	isResponses := modelFormat(model) == "responses"
	targetURL := strings.TrimRight(upstreamBase, "/") + "/chat/completions"
	format := "chat"
	var payload map[string]interface{}

	if strings.Contains(model, "space-bunny") {
		payload = map[string]interface{}{
			"model":    model,
			"messages": []map[string]string{{"role": "user", "content": "ping"}},
		}
	} else if isResponses {
		targetURL = strings.TrimRight(upstreamBase, "/") + "/responses"
		format = "responses"
		payload = map[string]interface{}{
			"model":              model,
			"input":              []map[string]interface{}{{"role": "developer", "content": sysPromptMuse}, {"role": "user", "content": "ping"}},
			"tools":              baseToolsMuse,
			"tool_choice":        "auto",
			"stream":             true,
			"max_output_tokens": 32,
		}
	} else {
		payload = map[string]interface{}{
			"model":      model,
			"messages":   []map[string]interface{}{{"role": "system", "content": sysPromptChat}, {"role": "user", "content": "ping"}},
			"tools":      baseToolsChat,
			"stream":     true,
			"max_tokens": 32,
		}
	}

	bodyBytes, _ := json.Marshal(payload)
	upReq, reqErr := http.NewRequest("POST", targetURL, bytes.NewReader(bodyBytes))
	if reqErr != nil {
		recordObserved(model, false)
		addLog(fmt.Sprintf("test %s request error: %v", model, reqErr))
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":     false,
			"model":  model,
			"format": format,
			"status": 400,
			"ms":     0,
			"detail": reqErr.Error(),
		})
		return
	}
	upReq.Header = headers

	client := getUpstreamClient(15 * time.Second)
	upResp, err := client.Do(upReq)
	duration := time.Since(start).Milliseconds()

	if err != nil {
		recordObserved(model, false)
		addLog(fmt.Sprintf("test %s failed: %v", model, err))
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":     false,
			"model":  model,
			"format": format,
			"status": 502,
			"ms":     duration,
			"detail": err.Error(),
		})
		return
	}
	defer upResp.Body.Close()

	bodyResp, _ := io.ReadAll(upResp.Body)
	ok := upResp.StatusCode == 200
	detail := string(bodyResp)
	if len(detail) > 100 {
		detail = detail[:100] + "..."
	}

	recordObserved(model, ok)
	go savePersistedSync()
	addLog(fmt.Sprintf("tested model %s: status=%d ms=%d", model, upResp.StatusCode, duration))
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     ok,
		"model":  model,
		"format": format,
		"status": upResp.StatusCode,
		"ms":     duration,
		"detail": detail,
	})
}

func handleApiSync(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if limited, retryAfter := rateLimitFor(r); limited {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"type":    "rate_limit_error",
				"message": "Too many requests. Please slow down.",
			},
		})
		return
	}
	if !adminAuth(w, r) {
		return
	}
	go syncModels()
	go syncModelsDevMetadata(true)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":      true,
		"message": "sync triggered",
	})
}

func handleApiReset(w http.ResponseWriter, r *http.Request) {
	if !adminAuth(w, r) {
		return
	}
	statsMu.Lock()
	totalReqs = 0
	totalErrors = 0
	recentHistory = nil
	window60 = nil
	perMinute = make(map[int64]int64)
	statsMu.Unlock()

	observedMu.Lock()
	observed = make(map[string]map[string]int64)
	observedMu.Unlock()
	go savePersistedSync()

	addLog("stats reset via UI")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if limited, retryAfter := rateLimitFor(r); limited {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"type":    "rate_limit_error",
				"message": "Too many requests. Please slow down.",
			},
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	var reqBody map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		recordReq(400, "unknown", time.Since(start).Milliseconds())
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"invalid JSON body"}}`, http.StatusBadRequest)
		return
	}

	rawModel, _ := reqBody["model"].(string)
	model := resolveModelName(rawModel)
	isStream, _ := reqBody["stream"].(bool)

	// Auth token & BYOK resolution
	inAuth := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(inAuth, "Bearer "))
	apiKey := strings.TrimSpace(r.Header.Get("x-api-key"))
	if token == "" && apiKey != "" {
		token = apiKey
	}
	upstreamHeader := strings.TrimSpace(r.Header.Get("x-zen-key"))

	cfgMu.RLock()
	proxyKey := cfg.ProxyKey
	defaultUpstreamKey := cfg.DefaultUpstreamKey
	cfgMu.RUnlock()

	isByok := strings.HasPrefix(token, "zen_") || strings.HasPrefix(token, "oc_") ||
		strings.HasPrefix(upstreamHeader, "zen_") || strings.HasPrefix(upstreamHeader, "oc_")

	if proxyKey != "" {
		if subtle.ConstantTimeCompare([]byte(token), []byte(proxyKey)) != 1 && !isByok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"invalid proxy key"}}`))
			return
		}
	}

	auth := "Bearer public"
	if upstreamHeader != "" && upstreamHeader != "public" {
		auth = "Bearer " + upstreamHeader
	} else if isByok {
		auth = inAuth
	} else if defaultUpstreamKey != "" {
		auth = "Bearer " + defaultUpstreamKey
	} else if token != "" && token != "public" && proxyKey == "" {
		auth = inAuth
	}

	sessionID := genSessionID()
	requestID := genRequestID()

	cfgMu.RLock()
	ua := cfg.UA
	timeoutSec := cfg.TimeoutMs / 1000
	if timeoutSec <= 0 {
		timeoutSec = 120
	}
	upstreamBase := cfg.Upstream
	cfgMu.RUnlock()

	headers := http.Header{
		"Authorization":     []string{auth},
		"Content-Type":      []string{"application/json"},
		"User-Agent":        []string{ua},
		"x-opencode-client":  []string{"cli"},
		"x-opencode-project": []string{"global"},
		"x-opencode-session": []string{sessionID},
		"x-opencode-request": []string{requestID},
		"Accept":            []string{"*/*"},
	}

	isResponses := modelFormat(model) == "responses"
	targetURL := strings.TrimRight(upstreamBase, "/") + "/chat/completions"
	if isResponses {
		targetURL = strings.TrimRight(upstreamBase, "/") + "/responses"
	}

	var upstreamPayload map[string]interface{}

	if strings.Contains(model, "space-bunny") {
		upstreamPayload = reqBody
		upstreamPayload["model"] = model
		upstreamPayload["stream"] = true
	} else if isResponses {
		messages, _ := reqBody["messages"].([]interface{})
		var userSys []string
		var inputItems []map[string]interface{}

		for _, m := range messages {
			msg, ok := m.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			content := extractContentText(msg["content"])
			if role == "system" {
				userSys = append(userSys, content)
			}
		}

		combSys := sysPromptMuse
		if len(userSys) > 0 {
			combSys += "\n\n# User Instructions & Guidelines:\n" + strings.Join(userSys, "\n")
		}
		inputItems = append(inputItems, map[string]interface{}{"role": "developer", "content": combSys})

		for _, m := range messages {
			msg, ok := m.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			content := extractContentText(msg["content"])
			if role == "system" {
				continue
			}
			if role == "tool" {
				callID, _ := msg["tool_call_id"].(string)
				inputItems = append(inputItems, map[string]interface{}{
					"type":    "function_call_output",
					"call_id": callID,
					"output":  content,
				})
			} else if role == "assistant" {
				if content != "" {
					inputItems = append(inputItems, map[string]interface{}{"role": "assistant", "content": content})
				}
				if tcs, ok := msg["tool_calls"].([]interface{}); ok {
					for _, tcItem := range tcs {
						if tc, ok := tcItem.(map[string]interface{}); ok {
							fn, _ := tc["function"].(map[string]interface{})
							fnName, _ := fn["name"].(string)
							fnArgs, _ := fn["arguments"].(string)
							callID, _ := tc["id"].(string)
							inputItems = append(inputItems, map[string]interface{}{
								"type":      "function_call",
								"call_id":   callID,
								"name":      fnName,
								"arguments": fnArgs,
							})
						}
					}
				}
			} else {
				inputItems = append(inputItems, map[string]interface{}{"role": role, "content": content})
			}
		}

		mergedTools := make([]map[string]interface{}, len(baseToolsMuse))
		copy(mergedTools, baseToolsMuse)
		existingNames := make(map[string]bool)
		for _, t := range mergedTools {
			if n, ok := t["name"].(string); ok {
				existingNames[n] = true
			}
		}
		if inTools, ok := reqBody["tools"].([]interface{}); ok {
			for _, t := range inTools {
				if tm, ok := t.(map[string]interface{}); ok {
					if fn, ok := tm["function"].(map[string]interface{}); ok {
						fnName, _ := fn["name"].(string)
						if !existingNames[fnName] {
							mergedTools = append(mergedTools, map[string]interface{}{
								"type":        "function",
								"name":        fnName,
								"description": fn["description"],
								"parameters":  fn["parameters"],
								"strict":       false,
							})
							existingNames[fnName] = true
						}
					}
				}
			}
		}

		upstreamPayload = map[string]interface{}{
			"model":              model,
			"input":              inputItems,
			"tools":              mergedTools,
			"tool_choice":        "auto",
			"stream":             true,
			"max_output_tokens": 32000,
			"store":              false,
			"include":            []string{"reasoning.encrypted_content"},
			"prompt_cache_key":   sessionID,
		}
	} else {
		messages, _ := reqBody["messages"].([]interface{})
		var userSys []string
		var otherMsgs []interface{}

		for _, m := range messages {
			msg, ok := m.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			content := extractContentText(msg["content"])
			if role == "system" {
				userSys = append(userSys, content)
			} else {
				otherMsgs = append(otherMsgs, m)
			}
		}

		combSys := sysPromptChat
		if len(userSys) > 0 {
			combSys += "\n\n# User Instructions & Guidelines:\n" + strings.Join(userSys, "\n")
		}

		upMsgs := []interface{}{map[string]interface{}{"role": "system", "content": combSys}}
		upMsgs = append(upMsgs, otherMsgs...)

		mergedTools := make([]map[string]interface{}, len(baseToolsChat))
		copy(mergedTools, baseToolsChat)
		existingNames := make(map[string]bool)
		for _, t := range mergedTools {
			if fn, ok := t["function"].(map[string]interface{}); ok {
				if n, ok := fn["name"].(string); ok {
					existingNames[n] = true
				}
			}
		}
		if inTools, ok := reqBody["tools"].([]interface{}); ok {
			for _, t := range inTools {
				if tm, ok := t.(map[string]interface{}); ok {
					if fn, ok := tm["function"].(map[string]interface{}); ok {
						fnName, _ := fn["name"].(string)
						if !existingNames[fnName] {
							mergedTools = append(mergedTools, tm)
							existingNames[fnName] = true
						}
					}
				}
			}
		}

		upstreamPayload = map[string]interface{}{
			"model":      model,
			"messages":   upMsgs,
			"tools":      mergedTools,
			"stream":     true,
			"max_tokens": 32000,
		}
	}

	bodyBytes, _ := json.Marshal(upstreamPayload)
	upReq, err := http.NewRequest("POST", targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		recordReq(500, model, time.Since(start).Milliseconds())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	upReq.Header = headers

	client := getUpstreamClient(time.Duration(timeoutSec) * time.Second)
	upResp, err := client.Do(upReq)
	if err != nil {
		recordReq(502, model, time.Since(start).Milliseconds())
		http.Error(w, fmt.Sprintf(`{"error":{"type":"upstream_error","message":%q}}`, err.Error()), http.StatusBadGateway)
		return
	}
	defer upResp.Body.Close()

	if upResp.StatusCode != 200 {
		recordReq(upResp.StatusCode, model, time.Since(start).Milliseconds())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upResp.StatusCode)
		io.Copy(w, upResp.Body)
		return
	}

	// Stream
	if isStream {
		recordReq(200, model, time.Since(start).Milliseconds())
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		reader := bufio.NewReader(upResp.Body)

		if isResponses {
			randHex := make([]byte, 6)
			rand.Read(randHex)
			compID := "chatcmpl-" + hex.EncodeToString(randHex)
			for {
				line, err := reader.ReadString('\n')
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "data: ") {
					var event map[string]interface{}
					if err := json.Unmarshal([]byte(strings.TrimPrefix(trimmed, "data: ")), &event); err == nil {
						eType, _ := event["type"].(string)
						if eType == "response.output_text.delta" {
							deltaText, _ := event["delta"].(string)
							chunk, _ := json.Marshal(map[string]interface{}{
								"id":      compID,
								"object":  "chat.completion.chunk",
								"created": time.Now().Unix(),
								"model":   model,
								"choices": []map[string]interface{}{
									{"index": 0, "delta": map[string]interface{}{"content": deltaText}, "finish_reason": nil},
								},
							})
							fmt.Fprintf(w, "data: %s\n\n", string(chunk))
							if ok {
								flusher.Flush()
							}
						} else if eType == "response.function_call_arguments.delta" {
							deltaArgs, _ := event["delta"].(string)
							chunk, _ := json.Marshal(map[string]interface{}{
								"id":      compID,
								"object":  "chat.completion.chunk",
								"created": time.Now().Unix(),
								"model":   model,
								"choices": []map[string]interface{}{
									{"index": 0, "delta": map[string]interface{}{
										"tool_calls": []map[string]interface{}{
											{"index": 0, "function": map[string]interface{}{"arguments": deltaArgs}},
										},
									}, "finish_reason": nil},
								},
							})
							fmt.Fprintf(w, "data: %s\n\n", string(chunk))
							if ok {
								flusher.Flush()
							}
						} else if eType == "response.output_item.added" {
							if item, ok := event["item"].(map[string]interface{}); ok {
								if item["type"] == "function_call" {
									callID, _ := item["call_id"].(string)
									fnName, _ := item["name"].(string)
									chunk, _ := json.Marshal(map[string]interface{}{
										"id":      compID,
										"object":  "chat.completion.chunk",
										"created": time.Now().Unix(),
										"model":   model,
										"choices": []map[string]interface{}{
											{"index": 0, "delta": map[string]interface{}{
												"tool_calls": []map[string]interface{}{
													{
														"index":    0,
														"id":       callID,
														"type":     "function",
														"function": map[string]interface{}{"name": fnName, "arguments": ""},
													},
												},
											}, "finish_reason": nil},
										},
									})
									fmt.Fprintf(w, "data: %s\n\n", string(chunk))
									if ok {
										flusher.Flush()
									}
								}
							}
						} else if eType == "response.completed" {
							respMap, _ := event["response"].(map[string]interface{})
							outItems, _ := respMap["output"].([]interface{})
							hasToolCall := false
							for _, it := range outItems {
								if im, ok := it.(map[string]interface{}); ok && im["type"] == "function_call" {
									hasToolCall = true
									break
								}
							}
							fr := "stop"
							if hasToolCall {
								fr = "tool_calls"
							}
							chunk, _ := json.Marshal(map[string]interface{}{
								"id":      compID,
								"object":  "chat.completion.chunk",
								"created": time.Now().Unix(),
								"model":   model,
								"choices": []map[string]interface{}{
									{"index": 0, "delta": map[string]interface{}{}, "finish_reason": fr},
								},
							})
							fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", string(chunk))
							if ok {
								flusher.Flush()
							}
							break
						}
					}
				}
				if err != nil {
					break
				}
			}
			return
		}

		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				w.Write(line)
				if ok {
					flusher.Flush()
				}
			}
			if err != nil {
				break
			}
		}
		return
	}

	// Non-streaming: accumulate
	reader := bufio.NewReader(upResp.Body)
	var fullText strings.Builder
	toolCallsMap := make(map[int]map[string]interface{})
	finishReason := "stop"

	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
			jsonStr := strings.TrimPrefix(line, "data: ")
			var parsed map[string]interface{}
			if err := json.Unmarshal([]byte(jsonStr), &parsed); err == nil {
				if isResponses {
					eType, _ := parsed["type"].(string)
					if eType == "response.output_text.delta" {
						if d, ok := parsed["delta"].(string); ok {
							fullText.WriteString(d)
						}
					} else if eType == "response.output_item.added" {
						if item, ok := parsed["item"].(map[string]interface{}); ok {
							if item["type"] == "function_call" {
								finishReason = "tool_calls"
								callID, _ := item["call_id"].(string)
								fnName, _ := item["name"].(string)
								toolCallsMap[0] = map[string]interface{}{
									"id":   callID,
									"type": "function",
									"function": map[string]interface{}{
										"name":      fnName,
										"arguments": "",
									},
								}
							}
						}
					} else if eType == "response.function_call_arguments.delta" {
						if d, ok := parsed["delta"].(string); ok {
							if _, exists := toolCallsMap[0]; exists {
								targetFn := toolCallsMap[0]["function"].(map[string]interface{})
								targetFn["arguments"] = targetFn["arguments"].(string) + d
							}
						}
					}
				} else {
					if choices, ok := parsed["choices"].([]interface{}); ok && len(choices) > 0 {
						if ch, ok := choices[0].(map[string]interface{}); ok {
							delta, _ := ch["delta"].(map[string]interface{})
							if delta != nil {
								if c, ok := delta["content"].(string); ok {
									fullText.WriteString(c)
								}
								if tcs, ok := delta["tool_calls"].([]interface{}); ok {
									finishReason = "tool_calls"
									for _, tcItem := range tcs {
										if tcMap, ok := tcItem.(map[string]interface{}); ok {
											idx := 0
											if idxVal, ok := tcMap["index"].(float64); ok {
												idx = int(idxVal)
											}
											if _, exists := toolCallsMap[idx]; !exists {
												randHex := make([]byte, 6)
												rand.Read(randHex)
												idVal, _ := tcMap["id"].(string)
												if idVal == "" {
													idVal = "call_" + hex.EncodeToString(randHex)
												}
												toolCallsMap[idx] = map[string]interface{}{
													"id":   idVal,
													"type": "function",
													"function": map[string]interface{}{
														"name":      "",
														"arguments": "",
													},
												}
											}
											fn, _ := tcMap["function"].(map[string]interface{})
											if fn != nil {
												targetFn := toolCallsMap[idx]["function"].(map[string]interface{})
												if n, ok := fn["name"].(string); ok {
													targetFn["name"] = targetFn["name"].(string) + n
												}
												if a, ok := fn["arguments"].(string); ok {
													targetFn["arguments"] = targetFn["arguments"].(string) + a
												}
											}
										}
									}
								}
							}
							if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
							}
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}

	randHex := make([]byte, 6)
	rand.Read(randHex)

	msgObj := map[string]interface{}{
		"role": "assistant",
	}
	if fullText.Len() > 0 {
		msgObj["content"] = fullText.String()
	} else if len(toolCallsMap) == 0 {
		msgObj["content"] = ""
	}

	if len(toolCallsMap) > 0 {
		var toolCallsList []map[string]interface{}
		var keys []int
		for k := range toolCallsMap {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		for _, k := range keys {
			toolCallsList = append(toolCallsList, toolCallsMap[k])
		}
		msgObj["tool_calls"] = toolCallsList
		finishReason = "tool_calls"
	}

	respObj := map[string]interface{}{
		"id":      "chatcmpl-" + hex.EncodeToString(randHex),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       msgObj,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     100,
			"completion_tokens": len(strings.Fields(fullText.String())),
			"total_tokens":      100 + len(strings.Fields(fullText.String())),
		},
	}

	recordReq(200, model, time.Since(start).Milliseconds())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(respObj)
}

func main() {
	port := "8787"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}

	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		dir = "."
	}
	if err := loadResources(dir); err != nil {
		log.Printf("Warn loading resources: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/config", handleApiConfig)
	mux.HandleFunc("/api/test", handleApiTest)
	mux.HandleFunc("/api/logs", handleLogs)
	mux.HandleFunc("/api/sync", handleApiSync)
	mux.HandleFunc("/api/reset", handleApiReset)

	loadPersistedSync()
	loadPersistedMeta()
	go syncModelsDevMetadata(false)
	startSyncScheduler()
	startUAScheduler()

	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/models", handleModels)
	mux.HandleFunc("/v1/chat/completions", handleChat)
	mux.HandleFunc("/chat/completions", handleChat)
	mux.HandleFunc("/v1/messages", handleAnthropicMessages)
	mux.HandleFunc("/messages", handleAnthropicMessages)

	// Static assets handler (natively sanitized against traversal)
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir(assetsDir))))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" || r.URL.Path == "/ui" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if len(htmlIndex) > 0 {
				w.Write(htmlIndex)
				return
			}
			w.Write([]byte("<h1>OpenCode Router</h1><p>Running clean.</p>"))
			return
		}
		http.NotFound(w, r)
	})

	server := &http.Server{
		Addr:    "0.0.0.0:" + port,
		Handler: mux,
	}

	addLog(fmt.Sprintf("opencode-router listening on http://0.0.0.0:%s", port))
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}


var (
	rateBucketsMu sync.Mutex
	rateBuckets   = make(map[string][]int64)
)

func clientIP(r *http.Request) string {
	cfgMu.RLock()
	trust := cfg.TrustForwarded
	cfgMu.RUnlock()
	if trust {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[0])
		}
		if xri := r.Header.Get("X-Real-IP"); xri != "" {
			return strings.TrimSpace(xri)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func rateLimitFor(r *http.Request) (bool, int) {
	cfgMu.RLock()
	max := cfg.RateLimitMax
	window := cfg.RateLimitWindowMs
	cfgMu.RUnlock()

	if max <= 0 {
		return false, 0
	}
	if window <= 0 {
		window = 60000
	}

	ip := clientIP(r)
	if ip == "" {
		ip = "unknown"
	}

	now := time.Now().UnixMilli()
	rateBucketsMu.Lock()
	defer rateBucketsMu.Unlock()

	hits := rateBuckets[ip]
	validIdx := 0
	for validIdx < len(hits) && (now-hits[validIdx] >= int64(window)) {
		validIdx++
	}
	hits = hits[validIdx:]
	if len(hits) == 0 {
		delete(rateBuckets, ip)
	}

	if len(hits) >= max {
		retryAfter := int((hits[0] + int64(window) - now) / 1000)
		if retryAfter < 1 {
			retryAfter = 1
		}
		rateBuckets[ip] = hits
		return true, retryAfter
	}

	hits = append(hits, now)
	rateBuckets[ip] = hits
	return false, 0
}
