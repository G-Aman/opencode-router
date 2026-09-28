package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"
)

var (
	uaAutoMu    sync.RWMutex
	uaAutoState = struct {
		at      int64
		version string
	}{}
	versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+`)
	opencodeUARe = regexp.MustCompile(`^opencode/\d+\.\d+\.\d+`)
)

func refreshUA(force bool) string {
	cfgMu.RLock()
	autoUA := cfg.AutoUA
	uaRefreshMs := int64(cfg.UaRefreshMs)
	if uaRefreshMs <= 0 {
		uaRefreshMs = 86400000 // 24h default
	}
	currentUA := cfg.UA
	cfgMu.RUnlock()

	if !autoUA {
		return ""
	}

	now := time.Now().UnixMilli()
	uaAutoMu.RLock()
	if !force && uaAutoState.at > 0 && (now-uaAutoState.at) < uaRefreshMs {
		v := uaAutoState.version
		uaAutoMu.RUnlock()
		return v
	}
	uaAutoMu.RUnlock()

	uaAutoMu.Lock()
	defer uaAutoMu.Unlock()

	uaAutoState.at = now

	req, err := http.NewRequest("GET", "https://registry.npmjs.org/opencode-ai/latest", nil)
	if err != nil {
		return uaAutoState.version
	}
	req.Header.Set("Accept", "application/json")

	client := getUpstreamClient(10 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return uaAutoState.version
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return uaAutoState.version
	}

	var parsed struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return uaAutoState.version
	}

	v := parsed.Version
	if !versionRe.MatchString(v) {
		return uaAutoState.version
	}

	uaAutoState.version = v
	// Replace only the opencode/<version> prefix, preserving the full SDK / runtime suffix
	prefixRe := regexp.MustCompile(`^opencode/\d+\.\d+\.\d+`)
	var nextUA string
	if prefixRe.MatchString(currentUA) {
		nextUA = prefixRe.ReplaceAllString(currentUA, fmt.Sprintf("opencode/%s", v))
	} else {
		nextUA = fmt.Sprintf("opencode/%s", v)
	}

	if nextUA != currentUA {
		cfgMu.Lock()
		cfg.UA = nextUA
		if b, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			os.WriteFile(configPath, b, 0644)
		}
		cfgMu.Unlock()
		addLog(fmt.Sprintf("auto-UA: opencode %s released — updated User-Agent to %s", v, nextUA))
	}

	return v
}

func startUAScheduler() {
	go func() {
		cfgMu.RLock()
		autoUA := cfg.AutoUA
		uaRefreshMs := cfg.UaRefreshMs
		cfgMu.RUnlock()

		if uaRefreshMs <= 0 {
			uaRefreshMs = 86400000 // 24 hours
		}

		if autoUA {
			refreshUA(true)
		}

		ticker := time.NewTicker(time.Duration(uaRefreshMs) * time.Millisecond)
		defer ticker.Stop()

		for range ticker.C {
			cfgMu.RLock()
			autoUA := cfg.AutoUA
			cfgMu.RUnlock()
			if autoUA {
				refreshUA(false)
			}
		}
	}()
}
