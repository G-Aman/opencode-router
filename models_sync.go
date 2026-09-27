package main

import (
	"bytes"
	"os"
	"path/filepath"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
		"regexp"
	"strings"
	"sync"
	"time"
)

var (
	validModelIDRe = regexp.MustCompile(`^[A-Za-z0-9._:@+/%-]+$`)
	notChatRe      = []*regexp.Regexp{regexp.MustCompile(`^jev-`)}

	modelsCacheMu sync.RWMutex
	modelsCache   = struct {
		at   int64
		data []map[string]interface{}
		ok   bool
	}{}

	syncStateMu sync.Mutex
	syncState   = struct {
		ok          bool
		at          int64
		running     bool
		ms          int64
		working     []string
		rateLimited []string
		gated       []string
		flaky       []string
		dead        []string
		error       string
	}{
		ok:          false,
		at:          0,
		working:     []string{},
		rateLimited: []string{},
		gated:       []string{},
		flaky:       []string{},
		dead:        []string{},
	}

	modelHealthMu sync.Mutex
	modelHealth   = make(map[string]int)
)

func isFreeModel(id string) bool {
	if !validModelIDRe.MatchString(id) {
		return false
	}
	for _, re := range notChatRe {
		if re.MatchString(id) {
			return false
		}
	}
	return true
}

func getAllowedSet() map[string]bool {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	disabled := make(map[string]bool)
	for _, d := range cfg.DisabledModels {
		disabled[d] = true
	}
	res := make(map[string]bool)
	for _, m := range cfg.FallbackModels {
		if !disabled[m] {
			res[m] = true
		}
	}
	for _, v := range cfg.ModelAliases {
		if !disabled[v] {
			res[v] = true
		}
	}
	return res
}

func effectiveDefault() string {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	if cfg.DefaultModel != "" {
		return cfg.DefaultModel
	}
	syncStateMu.Lock()
	synced := make(map[string]bool)
	for _, m := range syncState.working {
		synced[m] = true
	}
	for _, m := range syncState.rateLimited {
		synced[m] = true
	}
	syncStateMu.Unlock()

	for _, m := range cfg.FallbackModels {
		if synced[m] {
			return m
		}
	}
	if len(cfg.FallbackModels) > 0 {
		return cfg.FallbackModels[0]
	}
	return "nemotron-3-ultra-free"
}

func fetchModels() ([]map[string]interface{}, bool) {
	cfgMu.RLock()
	cacheMs := int64(cfg.CacheMs)
	if cacheMs <= 0 {
		cacheMs = 120000
	}
	upstream := cfg.Upstream
	ua := cfg.UA
	cfgMu.RUnlock()

	modelsCacheMu.RLock()
	now := time.Now().UnixMilli()
	if modelsCache.at > 0 && (now-modelsCache.at) < cacheMs && len(modelsCache.data) > 0 {
		data := modelsCache.data
		ok := modelsCache.ok
		modelsCacheMu.RUnlock()
		return data, ok
	}
	modelsCacheMu.RUnlock()

	modelsCacheMu.Lock()
	defer modelsCacheMu.Unlock()

	// Double check
	now = time.Now().UnixMilli()
	if modelsCache.at > 0 && (now-modelsCache.at) < cacheMs && len(modelsCache.data) > 0 {
		return modelsCache.data, modelsCache.ok
	}

	req, err := http.NewRequest("GET", fmt.Sprintf("%s/models", strings.TrimRight(upstream, "/")), nil)
	if err != nil {
		return modelsCache.data, false
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		// If upstream call fails, build dynamic catalog from configured fallbackModels & aliases
		data := buildFallbackModelList()
		modelsCache.at = now
		modelsCache.data = data
		modelsCache.ok = false
		return data, false
	}
	defer resp.Body.Close()

	var parsed struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		data := buildFallbackModelList()
		modelsCache.at = now
		modelsCache.data = data
		modelsCache.ok = false
		return data, false
	}

	allowed := getAllowedSet()
	syncStateMu.Lock()
	deadMap := make(map[string]bool)
	for _, m := range syncState.dead {
		deadMap[m] = true
	}
	liveMap := make(map[string]bool)
	for _, m := range syncState.working {
		liveMap[m] = true
	}
	for _, m := range syncState.rateLimited {
		liveMap[m] = true
	}
	syncStateMu.Unlock()

	var discoveredList []string
	for _, m := range parsed.Data {
		if id, ok := m["id"].(string); ok && strings.HasSuffix(id, "-free") && isFreeModel(id) {
			discoveredList = append(discoveredList, id)
		}
	}
	setUpstreamDiscovered(discoveredList)

	var free []map[string]interface{}
	seen := make(map[string]bool)

	for _, m := range parsed.Data {
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		// Strict filtering: Only models explicitly allowed in config AND not dead
		if allowed[id] && !deadMap[id] && isFreeModel(id) {
			// Merge rich metadata (context_window, max_tokens, modalities, etc.)
			meta := buildModelInfoMap(id)
			for k, v := range m {
				meta[k] = v
			}
			free = append(free, meta)
			seen[id] = true
		}
	}

	// Always ensure user configured fallbackModels and aliases are present
	cfgMu.RLock()
	for _, m := range cfg.FallbackModels {
		if !seen[m] && !deadMap[m] {
			free = append(free, buildModelInfoMap(m))
			seen[m] = true
		}
	}
	for aliasName, targetModel := range cfg.ModelAliases {
		if !seen[aliasName] {
			info := buildModelInfoMap(targetModel)
			info["id"] = aliasName
			free = append(free, info)
			seen[aliasName] = true
		}
		if !seen[targetModel] && !deadMap[targetModel] {
			free = append(free, buildModelInfoMap(targetModel))
			seen[targetModel] = true
		}
	}
	cfgMu.RUnlock()

	modelsCache.at = now
	modelsCache.data = free
	modelsCache.ok = true
	return free, true
}

func buildFallbackModelList() []map[string]interface{} {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	seen := make(map[string]bool)
	var list []map[string]interface{}

	for _, m := range cfg.FallbackModels {
		if !seen[m] {
			list = append(list, buildModelInfoMap(m))
			seen[m] = true
		}
	}
	for aliasName, targetModel := range cfg.ModelAliases {
		if !seen[aliasName] {
			info := buildModelInfoMap(targetModel)
			info["id"] = aliasName
			list = append(list, info)
			seen[aliasName] = true
		}
		if !seen[targetModel] {
			list = append(list, buildModelInfoMap(targetModel))
			seen[targetModel] = true
		}
	}
	for _, m := range modelCatalog {
		if !seen[m.ID] {
			list = append(list, buildModelInfoMap(m.ID))
			seen[m.ID] = true
		}
	}
	return list
}

func buildModelInfoMap(id string) map[string]interface{} {
	if dynamicMeta, ok := getDynamicModelMeta(id); ok {
		return dynamicMeta
	}
	for _, m := range modelCatalog {
		if m.ID == id {
			b, _ := json.Marshal(m)
			var res map[string]interface{}
			json.Unmarshal(b, &res)
			return res
		}
	}
	ctx := 128000
	maxTokens := 32000
	modalities := []string{"text"}
	supportsReason := false
	supportsTools := true

	if strings.Contains(id, "muse-spark") {
		ctx = 1048576
		maxTokens = 131072
		modalities = []string{"text", "image"}
	} else if strings.Contains(id, "nemotron") {
		ctx = 1000000
		maxTokens = 128000
		supportsReason = true
	} else if strings.Contains(id, "space-bunny") {
		ctx = 1048576
		maxTokens = 524288
		modalities = []string{"text", "image"}
		supportsReason = true
	} else if strings.Contains(id, "mimo") {
		ctx = 200000
		maxTokens = 32000
		supportsReason = true
	} else if strings.Contains(id, "ling") {
		ctx = 256000
		maxTokens = 32000
		supportsReason = true
	} else if strings.Contains(id, "longcat") {
		ctx = 524288
		maxTokens = 65536
		modalities = []string{"text", "image"}
		supportsReason = true
	} else if strings.Contains(id, "claude") {
		ctx = 200000
		maxTokens = 8192
		modalities = []string{"text", "image"}
		supportsTools = true
	} else if strings.Contains(id, "gpt-5") || strings.Contains(id, "gpt-6") {
		ctx = 256000
		maxTokens = 32000
		modalities = []string{"text", "image"}
		supportsReason = true
	} else if strings.Contains(id, "gemini") {
		ctx = 1048576
		maxTokens = 65536
		modalities = []string{"text", "image"}
	}

	return map[string]interface{}{
		"id":                 id,
		"object":             "model",
		"created":            time.Now().Unix(),
		"owned_by":           "opencode",
		"context_window":     ctx,
		"context_length":     ctx,
		"max_tokens":         maxTokens,
		"max_output_tokens":  maxTokens,
		"modalities":         modalities,
		"supports_tools":     supportsTools,
		"supports_reasoning": supportsReason,
		"pricing":            map[string]interface{}{"prompt": "0", "completion": "0"},
		"top_provider": map[string]interface{}{
			"context_length":        ctx,
			"max_completion_tokens": maxTokens,
		},
		"per_request_limits": map[string]interface{}{
			"context": ctx,
			"output":  maxTokens,
		},
	}
}

func probeModelUpstream(id string, auth string, session string) (*http.Response, string, error) {
	cfgMu.RLock()
	upstream := cfg.Upstream
	ua := cfg.UA
	timeout := cfg.TimeoutMs
	cfgMu.RUnlock()
	timeout = 8000 // Fast probe timeout like Node

	requestID := genRequestID()
	headers := http.Header{
		"Authorization":     []string{auth},
		"Content-Type":      []string{"application/json"},
		"User-Agent":        []string{ua},
		"x-opencode-client":  []string{"cli"},
		"x-opencode-project": []string{"global"},
		"x-opencode-session": []string{session},
		"x-opencode-request": []string{requestID},
		"Accept":            []string{"*/*"},
	}

	isMuse := strings.Contains(id, "muse-spark")
	targetURL := fmt.Sprintf("%s/chat/completions", strings.TrimRight(upstream, "/"))
	format := "chat"
	var payload map[string]interface{}

	if strings.Contains(id, "space-bunny") {
		payload = map[string]interface{}{
			"model":    id,
			"messages": []map[string]string{{"role": "user", "content": "ping"}},
		}
	} else if isMuse {
		targetURL = fmt.Sprintf("%s/responses", strings.TrimRight(upstream, "/"))
		format = "responses"
		payload = map[string]interface{}{
			"model":              id,
			"input":              []map[string]interface{}{{"role": "developer", "content": sysPromptMuse}, {"role": "user", "content": "ping"}},
			"tools":              baseToolsMuse,
			"tool_choice":        "auto",
			"stream":             true,
			"max_output_tokens": 32,
		}
	} else {
		payload = map[string]interface{}{
			"model":      id,
			"messages":   []map[string]interface{}{{"role": "system", "content": sysPromptChat}, {"role": "user", "content": "ping"}},
			"tools":      baseToolsChat,
			"stream":     true,
			"max_tokens": 32,
		}
	}

	bodyBytes, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, "", err
	}
	req.Header = headers

	client := &http.Client{Timeout: time.Duration(timeout) * time.Millisecond}
	resp, err := client.Do(req)
	return resp, format, err
}

func syncModels() {
	syncStateMu.Lock()
	if syncState.running {
		syncStateMu.Unlock()
		return
	}
	syncState.running = true
	syncState.at = time.Now().UnixMilli()
	start := time.Now()
	syncStateMu.Unlock()

	cfgMu.RLock()
	upstream := cfg.Upstream
	ua := cfg.UA
	defaultUpstreamKey := cfg.DefaultUpstreamKey
	currFallbacks := append([]string{}, cfg.FallbackModels...)
	cfgMu.RUnlock()

	req, err := http.NewRequest("GET", fmt.Sprintf("%s/models", strings.TrimRight(upstream, "/")), nil)
	if err != nil {
		finishSyncError(err.Error(), start)
		return
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		finishSyncError(err.Error(), start)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		finishSyncError(fmt.Sprintf("upstream /models → %d", resp.StatusCode), start)
		return
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		finishSyncError(err.Error(), start)
		return
	}

	var upstreamFree []string
	for _, m := range parsed.Data {
		if strings.HasSuffix(m.ID, "-free") && isFreeModel(m.ID) {
			upstreamFree = append(upstreamFree, m.ID)
		}
	}
	setUpstreamDiscovered(upstreamFree)

	candidateMap := make(map[string]bool)
	var candidates []string
	for _, m := range currFallbacks {
		if isFreeModel(m) && !candidateMap[m] {
			candidateMap[m] = true
			candidates = append(candidates, m)
		}
	}
	for _, m := range upstreamFree {
		if !candidateMap[m] {
			candidateMap[m] = true
			candidates = append(candidates, m)
		}
	}

	auth := "Bearer public"
	if defaultUpstreamKey != "" {
		auth = fmt.Sprintf("Bearer %s", defaultUpstreamKey)
	}

	var working, rateLimited, dead, flaky, gated []string
	var mu sync.Mutex
	removeFromCurrent := make(map[string]bool)

	// Probe worker pool (concurrency = 3)
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup

	for _, cand := range candidates {
		id := cand
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			sessID := genSessionID()
			r, _, err := probeModelUpstream(id, auth, sessID)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				modelHealthMu.Lock()
				modelHealth[id]++
				fails := modelHealth[id]
				modelHealthMu.Unlock()
				if fails >= 3 {
					dead = append(dead, id)
					removeFromCurrent[id] = true
				} else {
					flaky = append(flaky, id)
				}
				return
			}
			defer r.Body.Close()

			bodyBytes, _ := io.ReadAll(r.Body)
			_ = string(bodyBytes)

			var j struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			json.Unmarshal(bodyBytes, &j)
			hasErrObj := j.Type != "" || j.Error.Type != "" || j.Error.Message != ""
			bodyErr := strings.TrimSpace(j.Type + " " + j.Error.Type + " " + j.Error.Message)
			if hasErrObj || r.StatusCode >= 400 {
				bodyErr = strings.TrimSpace(string(bodyBytes) + " " + bodyErr)
			}

			if r.StatusCode == 200 && !hasErrObj {
				working = append(working, id)
				modelHealthMu.Lock()
				modelHealth[id] = 0
				modelHealthMu.Unlock()
			} else if r.StatusCode == 429 && bodyErr == "" {
				rateLimited = append(rateLimited, id)
				modelHealthMu.Lock()
				modelHealth[id] = 0
				modelHealthMu.Unlock()
			} else {
				gone := r.StatusCode == 404 ||
					strings.Contains(strings.ToLower(bodyErr), "model_not_found") ||
					strings.Contains(strings.ToLower(bodyErr), "no such model") ||
					strings.Contains(strings.ToLower(bodyErr), "does not exist") ||
					strings.Contains(strings.ToLower(bodyErr), "not supported") ||
					strings.Contains(strings.ToLower(bodyErr), "unavailable")
				isGated := strings.Contains(strings.ToLower(bodyErr), "freetiererror") || strings.Contains(strings.ToLower(bodyErr), "free tier can only")
				authErr := strings.Contains(strings.ToLower(bodyErr), "autherror") || strings.Contains(strings.ToLower(bodyErr), "invalid api key")
				temporary := strings.Contains(strings.ToLower(bodyErr), "regionerror") || strings.Contains(strings.ToLower(bodyErr), "not available in your country") || strings.Contains(strings.ToLower(bodyErr), "rate") || strings.Contains(strings.ToLower(bodyErr), "overloaded")

				if gone {
					dead = append(dead, id)
					removeFromCurrent[id] = true
				} else if isGated {
					gated = append(gated, id)
				} else if authErr {
					flaky = append(flaky, id)
				} else if temporary {
					modelHealthMu.Lock()
					modelHealth[id] = 0
					modelHealthMu.Unlock()
					flaky = append(flaky, id)
				} else {
					modelHealthMu.Lock()
					modelHealth[id]++
					fails := modelHealth[id]
					modelHealthMu.Unlock()
					if fails >= 3 {
						dead = append(dead, id)
						removeFromCurrent[id] = true
					} else {
						flaky = append(flaky, id)
					}
				}
			}
		}()
	}
	wg.Wait()

	// Update newList
	var newList []string
	flakyMap := make(map[string]bool)
	for _, m := range flaky {
		flakyMap[m] = true
	}
	for _, id := range currFallbacks {
		if !removeFromCurrent[id] {
			newList = append(newList, id)
		}
	}
	cfgMu.RLock()
	disabledMap := make(map[string]bool)
	for _, d := range cfg.DisabledModels {
		disabledMap[d] = true
	}
	cfgMu.RUnlock()

	for _, id := range upstreamFree {
		if !contains(newList, id) && !removeFromCurrent[id] && !flakyMap[id] && !disabledMap[id] {
			newList = append(newList, id)
		}
	}
	for _, id := range working {
		if !contains(newList, id) && !disabledMap[id] {
			newList = append(newList, id)
		}
	}

	addLog(fmt.Sprintf("auto-sync: model health probed (%d ok, %d rate-limited, %d gated, %d flaky, %d dead)", len(working), len(rateLimited), len(gated), len(flaky), len(dead)))

	syncStateMu.Lock()
	syncState.working = emptyIfNil(working)
	syncState.rateLimited = emptyIfNil(rateLimited)
	syncState.flaky = emptyIfNil(flaky)
	syncState.gated = emptyIfNil(gated)
	syncState.dead = emptyIfNil(dead)
	syncState.error = ""
	syncState.ok = true
	syncState.ms = time.Since(start).Milliseconds()
	syncState.running = false
	syncStateMu.Unlock()

	// Invalidate model cache
	modelsCacheMu.Lock()
	modelsCache.at = 0
	modelsCacheMu.Unlock()
	savePersistedSync()
}

func finishSyncError(errMsg string, start time.Time) {
	syncStateMu.Lock()
	syncState.ok = false
	syncState.error = errMsg
	syncState.ms = time.Since(start).Milliseconds()
	syncState.running = false
	syncStateMu.Unlock()
	addLog(fmt.Sprintf("auto-sync failed: %s", errMsg))
}

func contains(arr []string, item string) bool {
	for _, x := range arr {
		if x == item {
			return true
		}
	}
	return false
}

// Background scheduler
func startSyncScheduler() {
	go func() {
		cfgMu.RLock()
		autoSync := cfg.AutoSync
		intervalMs := cfg.AutoSyncIntervalMs
		cfgMu.RUnlock()
		if intervalMs <= 0 {
			intervalMs = 1800000 // 30m
		}
		if autoSync {
			addLog(fmt.Sprintf("auto-sync enabled (every %d min) — probing free models…", intervalMs/60000))
			go syncModels()
		}

		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			go syncModelsDevMetadata(false)
			cfgMu.RLock()
			autoSync := cfg.AutoSync
			cfgMu.RUnlock()
			if autoSync {
				syncModels()
			}
		}
	}()
}

func invalidateModelsCache() {
	modelsCacheMu.Lock()
	modelsCache.at = 0
	modelsCacheMu.Unlock()
	savePersistedSync()
}

var (
	observedMu sync.Mutex
	observed   = make(map[string]map[string]int64)
)

func recordObserved(model string, ok bool) {
	if model == "" {
		return
	}
	observedMu.Lock()
	defer observedMu.Unlock()
	cur, exists := observed[model]
	if !exists {
		cur = map[string]int64{"ok": 0, "fail": 0, "last": 0}
		observed[model] = cur
	}
	if ok {
		cur["ok"]++
	} else {
		cur["fail"]++
	}
	cur["last"] = time.Now().UnixMilli()
}

func getObservedCopy() map[string]map[string]int64 {
	observedMu.Lock()
	defer observedMu.Unlock()
	res := make(map[string]map[string]int64)
	for k, v := range observed {
		copyV := make(map[string]int64)
		for vk, vv := range v {
			copyV[vk] = vv
		}
		res[k] = copyV
	}
	return res
}

func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

var (
	upstreamDiscoveredMu sync.RWMutex
	upstreamDiscovered   []string
)

func setUpstreamDiscovered(models []string) {
	upstreamDiscoveredMu.Lock()
	defer upstreamDiscoveredMu.Unlock()
	upstreamDiscovered = models
}

func getUpstreamDiscovered() []string {
	upstreamDiscoveredMu.RLock()
	defer upstreamDiscoveredMu.RUnlock()
	if upstreamDiscovered == nil {
		return []string{}
	}
	return append([]string{}, upstreamDiscovered...)
}

// Persistence for syncState and observed map across restarts
type PersistedSyncData struct {
	At          int64                       `json:"at"`
	Ok          bool                        `json:"ok"`
	Working     []string                    `json:"working"`
	RateLimited []string                    `json:"rateLimited"`
	Gated       []string                    `json:"gated"`
	Flaky       []string                    `json:"flaky"`
	Dead        []string                    `json:"dead"`
	Ms          int64                       `json:"ms"`
	Observed    map[string]map[string]int64 `json:"observed"`
}

func syncStateFilePath() string {
	dir := filepath.Dir(configPath)
	return filepath.Join(dir, "opencode-sync-cache.json")
}

func savePersistedSync() {
	syncStateMu.Lock()
	p := PersistedSyncData{
		At:          syncState.at,
		Ok:          syncState.ok,
		Working:     syncState.working,
		RateLimited: syncState.rateLimited,
		Gated:       syncState.gated,
		Flaky:       syncState.flaky,
		Dead:        syncState.dead,
		Ms:          syncState.ms,
		Observed:    getObservedCopy(),
	}
	syncStateMu.Unlock()

	b, err := json.MarshalIndent(p, "", "  ")
	if err == nil {
		os.WriteFile(syncStateFilePath(), b, 0644)
	}
}

func loadPersistedSync() {
	b, err := os.ReadFile(syncStateFilePath())
	if err != nil {
		return
	}
	var p PersistedSyncData
	if err := json.Unmarshal(b, &p); err != nil {
		return
	}

	syncStateMu.Lock()
	syncState.at = p.At
	syncState.ok = p.Ok
	syncState.working = emptyIfNil(p.Working)
	syncState.rateLimited = emptyIfNil(p.RateLimited)
	syncState.gated = emptyIfNil(p.Gated)
	syncState.flaky = emptyIfNil(p.Flaky)
	syncState.dead = emptyIfNil(p.Dead)
	syncState.ms = p.Ms
	syncStateMu.Unlock()

	if p.Observed != nil {
		observedMu.Lock()
		for k, v := range p.Observed {
			observed[k] = v
		}
		observedMu.Unlock()
	}
}
