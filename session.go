package main

// Session ID generation and mapping.
//
// The free-tier gate validates the x-opencode-session ID: it must match
// opencode's native format — "ses_" + 12 hex chars encoding the *inverted*
// creation timestamp (~r where r = nowMs*4096 + per-ms counter, 48-bit) +
// 14 random chars from [0-9A-Za-z]. IDs with implausible/aged timestamps are
// rejected with 429 FreeUsageLimitError.
//
// Strategies (config.sessionStrategy):
//   - per-request:  fresh ID per upstream call (previous default behaviour)
//   - sticky:       one shared ID rotated every sessionWindowMin minutes
//   - conversation: reuse one ID per conversation — keyed by an explicit
//                   X-Conversation-ID header when the caller sends one,
//                   else by a fingerprint of the request's conversation start
//                   (system + first user message). Entries expire after 1h idle.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	sesMu      sync.Mutex
	sesLastMS  int64
	sesCounter int64
)

// genSessionID replicates opencode's ID generator (UH(true): "ses_" + t2e()).
func genSessionID() string {
	sesMu.Lock()
	defer sesMu.Unlock()
	ms := time.Now().UnixMilli()
	if ms != sesLastMS {
		sesLastMS = ms
		sesCounter = 0
	}
	sesCounter++
	r := uint64(ms)*4096 + uint64(sesCounter)
	o := ^r & 0xFFFFFFFFFFFF // 48-bit two's complement of r, matching ~r on BigInt
	prefix := fmt.Sprintf("%012x", o)
	// F = "0123456789ABC...abc..." — charset is what matters for random chars.
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	randPart := make([]byte, 14)
	rand.Read(randPart)
	for i := range randPart {
		randPart[i] = chars[int(randPart[i])%62]
	}
	return "ses_" + prefix + string(randPart)
}

// pickSessionID chooses the upstream session ID per the configured strategy.
func pickSessionID(r *http.Request, reqBody map[string]interface{}) string {
	cfgMu.RLock()
	strategy := cfg.SessionStrategy
	window := cfg.SessionWindowMin
	cfgMu.RUnlock()
	if strings.TrimSpace(strategy) == "" {
		strategy = "per-request"
	}

	switch strategy {
	case "sticky":
		return stickySession(time.Duration(window) * time.Minute)
	case "conversation":
		if id := conversationSession(r, reqBody); id != "" {
			return id
		}
	}
	return genSessionID() // per-request, and fallback for conversation
}

// stickySession returns one shared ID, rotated after the window elapses.
var (
	stickyMu     sync.Mutex
	stickyID     string
	stickyIssued time.Time
)

func stickySession(window time.Duration) string {
	if window <= 0 {
		window = 10 * time.Minute
	}
	stickyMu.Lock()
	defer stickyMu.Unlock()
	if stickyID == "" || time.Since(stickyIssued) >= window {
		stickyID = genSessionID()
		stickyIssued = time.Now()
	}
	return stickyID
}

const (
	convIdleTTL  = time.Hour
	convMaxEnads = 512
)

// conversationKeyFromAnthropic adapts an Anthropic request into the generic
// {model, messages} shape the fingerprint/header mapping understands.
// Content stays interface{} — extractContentText handles string or block arrays.
func conversationKeyFromAnthropic(req AnthropicMessageRequest) map[string]interface{} {
	msgs := make([]interface{}, 0, len(req.Messages)+1)
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]interface{}{"role": m.Role, "content": m.Content})
	}
	if req.System != nil {
		msgs = append([]interface{}{map[string]interface{}{"role": "system", "content": req.System}}, msgs...)
	}
	return map[string]interface{}{"model": req.Model, "messages": msgs}
}

type convEntry struct {
	id       string
	lastUsed time.Time
}

var (
	convMu   sync.Mutex
	convMap  = map[string]*convEntry{}
	convKeys []string // insertion order for eviction
)

// conversationSession maps a conversation to a stable upstream session ID.
// Explicit header wins; otherwise fingerprint the conversation start.
func conversationSession(r *http.Request, reqBody map[string]interface{}) string {
	key := ""
	if cid := strings.TrimSpace(r.Header.Get("X-Conversation-Id")); cid != "" {
		key = "hdr:" + cid
	} else if cid := strings.TrimSpace(r.Header.Get("X-Session-Id")); cid != "" && !strings.HasPrefix(cid, "ses_") {
		key = "hdr:" + cid
	}
	if key == "" && reqBody != nil {
		key = "fp:" + conversationFingerprint(reqBody)
	}
	if key == "" {
		return "" // caller falls back to per-request
	}

	convMu.Lock()
	defer convMu.Unlock()
	now := time.Now()
	if e, ok := convMap[key]; ok && now.Sub(e.lastUsed) < convIdleTTL {
		e.lastUsed = now
		return e.id
	}
	// Evict oldest entries when full.
	if len(convMap) >= convMaxEnads {
		type kv struct {
			k string
			t time.Time
		}
		all := make([]kv, 0, len(convMap))
		for k, e := range convMap {
			all = append(all, kv{k, e.lastUsed})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
		for i := 0; i < len(all)/4; i++ {
			delete(convMap, all[i].k)
		}
	}
	id := genSessionID()
	convMap[key] = &convEntry{id: id, lastUsed: now}
	return id
}

// conversationFingerprint hashes the stable head of the conversation:
// model + first system message + first user message. Stateless clients
// resend full history each turn, so the head is identical across turns.
func conversationFingerprint(reqBody map[string]interface{}) string {
	model, _ := reqBody["model"].(string)
	messages, _ := reqBody["messages"].([]interface{})
	h := sha256.New()
	fmt.Fprint(h, model)
	var firstUser, firstSystem string
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if firstSystem == "" && role == "system" {
			firstSystem = extractContentText(msg["content"])
		}
		if firstUser == "" && role == "user" {
			firstUser = extractContentText(msg["content"])
		}
	}
	fmt.Fprint(h, firstSystem)
	fmt.Fprint(h, firstUser)
	return hex.EncodeToString(h.Sum(nil))[:32]
}
