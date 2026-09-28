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
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Anthropic Request Structures
type AnthropicMessageRequest struct {
	Model         string                   `json:"model"`
	Messages      []AnthropicMessage       `json:"messages"`
	System        interface{}              `json:"system,omitempty"` // string or []AnthropicContentBlock
	MaxTokens     int                      `json:"max_tokens,omitempty"`
	Stream        bool                     `json:"stream,omitempty"`
	Temperature   *float64                 `json:"temperature,omitempty"`
	TopP          *float64                 `json:"top_p,omitempty"`
	TopK          *int                     `json:"top_k,omitempty"`
	StopSequences []string                 `json:"stop_sequences,omitempty"`
	Tools         []AnthropicTool          `json:"tools,omitempty"`
	ToolChoice    interface{}              `json:"tool_choice,omitempty"`
}

type AnthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []AnthropicContentBlock
}

type AnthropicContentBlock struct {
	Type       string                 `json:"type"`
	Text       string                 `json:"text,omitempty"`
	Source     *AnthropicImageSource  `json:"source,omitempty"`
	ID         string                 `json:"id,omitempty"`
	Name       string                 `json:"name,omitempty"`
	Input      map[string]interface{} `json:"input,omitempty"`
	ToolUseID  string                 `json:"tool_use_id,omitempty"`
	Content    interface{}            `json:"content,omitempty"`
	IsError    bool                   `json:"is_error,omitempty"`
}

type AnthropicImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // e.g. "image/jpeg", "image/png"
	Data      string `json:"data"`       // base64 data
}

type AnthropicTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

// Anthropic Response Structures
type AnthropicMessageResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"` // "message"
	Role         string                  `json:"role"` // "assistant"
	Content      []AnthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   string                  `json:"stop_reason,omitempty"` // "end_turn", "max_tokens", "stop_sequence", "tool_use"
	StopSequence *string                 `json:"stop_sequence"`
	Usage        AnthropicUsage          `json:"usage"`
}

type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func sendAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    errType,
			"message": msg,
		},
	})
}

func parseAnthropicSystemPrompt(sys interface{}) string {
	if sys == nil {
		return ""
	}
	if s, ok := sys.(string); ok {
		return s
	}
	if blocks, ok := sys.([]interface{}); ok {
		var sb strings.Builder
		for _, b := range blocks {
			if m, ok := b.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					if sb.Len() > 0 {
						sb.WriteString("\n\n")
					}
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.WriteHeader(200)
		return
	}

	if r.Method != "POST" {
		sendAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}

	start := time.Now()
	limited, retryAfter := rateLimitFor(r)
	if limited {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		sendAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error", fmt.Sprintf("rate limit exceeded, retry after %ds", retryAfter))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		sendAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read body")
		return
	}

	var req AnthropicMessageRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		sendAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "malformed json request: "+err.Error())
		return
	}

	// Auth check: Support x-api-key, Authorization: Bearer, or optional open access
	authHeader := r.Header.Get("Authorization")
	apiKeyHeader := r.Header.Get("x-api-key")
	passedKey := strings.TrimSpace(apiKeyHeader)
	if passedKey == "" && strings.HasPrefix(authHeader, "Bearer ") {
		passedKey = strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	}

	cfgMu.RLock()
	proxyKey := cfg.ProxyKey
	defaultUpstreamKey := cfg.DefaultUpstreamKey
	upstreamBase := cfg.Upstream
	ua := cfg.UA
	timeoutMs := cfg.TimeoutMs
	cfgMu.RUnlock()

	var upstreamAuth string
	if proxyKey != "" {
		if passedKey == "" {
			sendAnthropicError(w, http.StatusUnauthorized, "authentication_error", "missing api key")
			return
		}
		if subtle.ConstantTimeCompare([]byte(passedKey), []byte(proxyKey)) == 1 {
			if defaultUpstreamKey != "" {
				upstreamAuth = "Bearer " + defaultUpstreamKey
			} else {
				upstreamAuth = "Bearer public"
			}
		} else if strings.HasPrefix(passedKey, "zen_") || strings.HasPrefix(passedKey, "oc_") {
			upstreamAuth = "Bearer " + passedKey
		} else {
			sendAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid api key")
			return
		}
	} else {
		if passedKey != "" && (strings.HasPrefix(passedKey, "zen_") || strings.HasPrefix(passedKey, "oc_")) {
			upstreamAuth = "Bearer " + passedKey
		} else if defaultUpstreamKey != "" {
			upstreamAuth = "Bearer " + defaultUpstreamKey
		} else {
			upstreamAuth = "Bearer public"
		}
	}

	model := resolveModelName(req.Model)

	// Translate Anthropic messages -> OpenAI chat completions format
	var openAIMessages []map[string]interface{}

	// System prompt
	sysPrompt := parseAnthropicSystemPrompt(req.System)
	if modelFormat(model) == "responses" {
		if sysPrompt != "" {
			sysPrompt = sysPromptMuse + "\n\n" + sysPrompt
		} else {
			sysPrompt = sysPromptMuse
		}
	} else if sysPromptChat != "" {
		if sysPrompt != "" {
			sysPrompt = sysPromptChat + "\n\n" + sysPrompt
		} else {
			sysPrompt = sysPromptChat
		}
	}
	if sysPrompt != "" {
		openAIMessages = append(openAIMessages, map[string]interface{}{
			"role":    "system",
			"content": sysPrompt,
		})
	}

	for _, msg := range req.Messages {
		role := msg.Role
		if role != "user" && role != "assistant" {
			role = "user"
		}

		if str, ok := msg.Content.(string); ok {
			openAIMessages = append(openAIMessages, map[string]interface{}{
				"role":    role,
				"content": str,
			})
			continue
		}

		if rawBlocks, ok := msg.Content.([]interface{}); ok {
			var textParts []string
			var contentArray []map[string]interface{}
			var toolCalls []map[string]interface{}

			for _, rb := range rawBlocks {
				blockMap, ok := rb.(map[string]interface{})
				if !ok {
					continue
				}
				bType, _ := blockMap["type"].(string)

				switch bType {
				case "text":
					if txt, ok := blockMap["text"].(string); ok {
						textParts = append(textParts, txt)
						contentArray = append(contentArray, map[string]interface{}{
							"type": "text",
							"text": txt,
						})
					}
				case "image":
					if src, ok := blockMap["source"].(map[string]interface{}); ok {
						mediaType, _ := src["media_type"].(string)
						data, _ := src["data"].(string)
						if mediaType != "" && data != "" {
							contentArray = append(contentArray, map[string]interface{}{
								"type": "image_url",
								"image_url": map[string]interface{}{
									"url": fmt.Sprintf("data:%s;base64,%s", mediaType, data),
								},
							})
						}
					}
				case "tool_use":
					tcID, _ := blockMap["id"].(string)
					tcName, _ := blockMap["name"].(string)
					tcInput, _ := blockMap["input"].(map[string]interface{})
					argBytes, _ := json.Marshal(tcInput)
					toolCalls = append(toolCalls, map[string]interface{}{
						"id":   tcID,
						"type": "function",
						"function": map[string]interface{}{
							"name":      tcName,
							"arguments": string(argBytes),
						},
					})
				case "tool_result":
					trID, _ := blockMap["tool_use_id"].(string)
					var trContent string
					if trStr, ok := blockMap["content"].(string); ok {
						trContent = trStr
					} else if trArr, ok := blockMap["content"].([]interface{}); ok {
						var sb strings.Builder
						for _, it := range trArr {
							if itMap, ok := it.(map[string]interface{}); ok {
								if itTxt, ok := itMap["text"].(string); ok {
									sb.WriteString(itTxt)
								}
							}
						}
						trContent = sb.String()
					}
					openAIMessages = append(openAIMessages, map[string]interface{}{
						"role":         "tool",
						"tool_call_id": trID,
						"content":      trContent,
					})
				}
			}

			if role == "assistant" && len(toolCalls) > 0 {
				asstMsg := map[string]interface{}{
					"role":       "assistant",
					"tool_calls": toolCalls,
				}
				if len(textParts) > 0 {
					asstMsg["content"] = strings.Join(textParts, "\n")
				} else {
					asstMsg["content"] = ""
				}
				openAIMessages = append(openAIMessages, asstMsg)
			} else if len(contentArray) > 0 {
				if len(contentArray) == 1 && contentArray[0]["type"] == "text" {
					openAIMessages = append(openAIMessages, map[string]interface{}{
						"role":    role,
						"content": contentArray[0]["text"],
					})
				} else {
					openAIMessages = append(openAIMessages, map[string]interface{}{
						"role":    role,
						"content": contentArray,
					})
				}
			} else if len(textParts) > 0 {
				openAIMessages = append(openAIMessages, map[string]interface{}{
					"role":    role,
					"content": strings.Join(textParts, "\n"),
				})
			}
		}
	}

	// Tools translation: Anthropic -> OpenAI tools
	var openAITools []map[string]interface{}
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			openAITools = append(openAITools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.InputSchema,
				},
			})
		}
	}

	// Merge system prompts and base tools according to model format
	var mergedTools []map[string]interface{}
	var userSys []string
	var finalMsgs []map[string]interface{}

	for _, m := range openAIMessages {
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if role == "system" {
			userSys = append(userSys, content)
		} else {
			finalMsgs = append(finalMsgs, m)
		}
	}

	combSys := sysPromptChat
	if len(userSys) > 0 {
		combSys += "\n\n# User Instructions & Guidelines:\n" + strings.Join(userSys, "\n")
	}

	upMsgs := []map[string]interface{}{{"role": "system", "content": combSys}}
	upMsgs = append(upMsgs, finalMsgs...)

	mergedTools = make([]map[string]interface{}, len(baseToolsChat))
	copy(mergedTools, baseToolsChat)
	existingNames := make(map[string]bool)
	for _, t := range mergedTools {
		if fn, ok := t["function"].(map[string]interface{}); ok {
			if n, ok := fn["name"].(string); ok {
				existingNames[n] = true
			}
		}
	}
	for _, t := range openAITools {
		if fn, ok := t["function"].(map[string]interface{}); ok {
			fnName, _ := fn["name"].(string)
			if !existingNames[fnName] {
				mergedTools = append(mergedTools, t)
				existingNames[fnName] = true
			}
		}
	}

	// Build upstream OpenAI payload
	upstreamPayload := map[string]interface{}{
		"model":      model,
		"messages":   upMsgs,
		"stream":     true,
		"tools":      mergedTools,
		"max_tokens": 32000,
	}

	sessionID := genSessionID()
	randBytes := make([]byte, 16)
	rand.Read(randBytes)
	requestID := hex.EncodeToString(randBytes)

	headers := http.Header{
		"Authorization":     []string{upstreamAuth},
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

	var payloadBytes []byte
	if isResponses {
		var inputItems []map[string]interface{}
		combSys := sysPromptMuse
		if sysPrompt != "" {
			combSys += "\n\n# User Instructions & Guidelines:\n" + sysPrompt
		}
		inputItems = append(inputItems, map[string]interface{}{"role": "developer", "content": combSys})

		for _, m := range openAIMessages {
			role, _ := m["role"].(string)
			if role == "system" {
				continue
			}
			content, _ := m["content"].(string)
			if role == "tool" {
				callID, _ := m["tool_call_id"].(string)
				inputItems = append(inputItems, map[string]interface{}{
					"type":    "function_call_output",
					"call_id": callID,
					"output":  content,
				})
			} else if role == "assistant" {
				if content != "" {
					inputItems = append(inputItems, map[string]interface{}{"role": "assistant", "content": content})
				}
				if tcs, ok := m["tool_calls"].([]map[string]interface{}); ok {
					for _, tc := range tcs {
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
		for _, t := range openAITools {
			if fn, ok := t["function"].(map[string]interface{}); ok {
				fnName, _ := fn["name"].(string)
				if !existingNames[fnName] {
					mergedTools = append(mergedTools, map[string]interface{}{
						"type":        "function",
						"name":        fnName,
						"description": fn["description"],
						"parameters":  fn["parameters"],
						"strict":      false,
					})
					existingNames[fnName] = true
				}
			}
		}

		responsesPayload := map[string]interface{}{
			"model":  model,
			"input":  inputItems,
			"stream": true,
			"tools":  mergedTools,
		}
		payloadBytes, _ = json.Marshal(responsesPayload)
	} else {
		payloadBytes, _ = json.Marshal(upstreamPayload)
	}

	upReq, reqErr := http.NewRequest("POST", targetURL, bytes.NewReader(payloadBytes))
	if reqErr != nil {
		sendAnthropicError(w, http.StatusInternalServerError, "api_error", reqErr.Error())
		return
	}
	upReq.Header = headers

	timeoutSec := timeoutMs / 1000
	if timeoutSec <= 0 {
		timeoutSec = 120
	}
	client := getUpstreamClient(time.Duration(timeoutSec) * time.Second)
	upResp, err := client.Do(upReq)
	if err != nil {
		recordReq(502, model, time.Since(start).Milliseconds())
		sendAnthropicError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("upstream error: %v", err))
		return
	}
	defer upResp.Body.Close()

	if upResp.StatusCode >= 400 {
		b, _ := io.ReadAll(upResp.Body)
		recordReq(upResp.StatusCode, model, time.Since(start).Milliseconds())
		sendAnthropicError(w, upResp.StatusCode, "api_error", fmt.Sprintf("upstream returned %d: %s", upResp.StatusCode, string(b)))
		return
	}

	recordReq(200, model, time.Since(start).Milliseconds())
	msgID := fmt.Sprintf("msg_%s", requestID[:24])

	// Non-Streaming Response
	if !req.Stream {
		if isResponses {
			// Read SSE stream from /responses endpoint and accumulate
			reader := bufio.NewReader(upResp.Body)
			var fullText strings.Builder
			var toolCallsList []AnthropicContentBlock
			stopReason := "end_turn"

			for {
				line, err := reader.ReadString('\n')
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "data: ") && trimmed != "data: [DONE]" {
					var event map[string]interface{}
					if err := json.Unmarshal([]byte(strings.TrimPrefix(trimmed, "data: ")), &event); err == nil {
						eType, _ := event["type"].(string)
						if eType == "response.output_text.delta" {
							if d, ok := event["delta"].(string); ok {
								fullText.WriteString(d)
							}
						} else if eType == "response.output_item.added" {
							if item, ok := event["item"].(map[string]interface{}); ok {
								if item["type"] == "function_call" {
									stopReason = "tool_use"
									callID, _ := item["call_id"].(string)
									fnName, _ := item["name"].(string)
									toolCallsList = append(toolCallsList, AnthropicContentBlock{
										Type:  "tool_use",
										ID:    callID,
										Name:  fnName,
										Input: make(map[string]interface{}),
									})
								}
							}
						} else if eType == "response.function_call_arguments.delta" {
							if d, ok := event["delta"].(string); ok && len(toolCallsList) > 0 {
								lastIdx := len(toolCallsList) - 1
								var curArgs string
								if existing, ok := toolCallsList[lastIdx].Input["_raw"].(string); ok {
									curArgs = existing + d
								} else {
									curArgs = d
								}
								toolCallsList[lastIdx].Input["_raw"] = curArgs
							}
						}
					}
				}
				if err != nil {
					break
				}
			}

			for i := range toolCallsList {
				if raw, ok := toolCallsList[i].Input["_raw"].(string); ok {
					var parsed map[string]interface{}
					json.Unmarshal([]byte(raw), &parsed)
					if parsed != nil {
						toolCallsList[i].Input = parsed
					} else {
						delete(toolCallsList[i].Input, "_raw")
					}
				}
			}

			var contentBlocks []AnthropicContentBlock
			if fullText.Len() > 0 {
				contentBlocks = append(contentBlocks, AnthropicContentBlock{
					Type: "text",
					Text: fullText.String(),
				})
			}
			contentBlocks = append(contentBlocks, toolCallsList...)
			if len(contentBlocks) == 0 {
				contentBlocks = append(contentBlocks, AnthropicContentBlock{Type: "text", Text: ""})
			}

			antResp := AnthropicMessageResponse{
				ID:           msgID,
				Type:         "message",
				Role:         "assistant",
				Content:      contentBlocks,
				Model:        req.Model,
				StopReason:   stopReason,
				StopSequence: nil,
				Usage: AnthropicUsage{
					InputTokens:  100,
					OutputTokens: fullText.Len() / 4,
				},
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			json.NewEncoder(w).Encode(antResp)
			return
		} else {
			// Read SSE stream from /chat/completions endpoint and accumulate
			reader := bufio.NewReader(upResp.Body)
			var fullText strings.Builder
			toolCallsMap := make(map[int]map[string]interface{})
			stopReason := "end_turn"

			for {
				line, err := reader.ReadString('\n')
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "data: ") && trimmed != "data: [DONE]" {
					var chunk struct {
						Choices []struct {
							Delta struct {
								Content   string `json:"content"`
								ToolCalls []struct {
									Index    int    `json:"index"`
									ID       string `json:"id"`
									Function struct {
										Name      string `json:"name"`
										Arguments string `json:"arguments"`
									} `json:"function"`
								} `json:"tool_calls"`
							} `json:"delta"`
							FinishReason string `json:"finish_reason"`
						} `json:"choices"`
					}
					if err := json.Unmarshal([]byte(strings.TrimPrefix(trimmed, "data: ")), &chunk); err == nil && len(chunk.Choices) > 0 {
						choice := chunk.Choices[0]
						if choice.Delta.Content != "" {
							fullText.WriteString(choice.Delta.Content)
						}
						for _, tc := range choice.Delta.ToolCalls {
							idx := tc.Index
							if _, exists := toolCallsMap[idx]; !exists {
								toolCallsMap[idx] = map[string]interface{}{
									"id":   tc.ID,
									"name": tc.Function.Name,
									"args": tc.Function.Arguments,
								}
							} else {
								cur := toolCallsMap[idx]
								if tc.ID != "" {
									cur["id"] = tc.ID
								}
								if tc.Function.Name != "" {
									cur["name"] = tc.Function.Name
								}
								cur["args"] = cur["args"].(string) + tc.Function.Arguments
							}
						}
						if choice.FinishReason == "tool_calls" {
							stopReason = "tool_use"
						} else if choice.FinishReason == "length" {
							stopReason = "max_tokens"
						}
					}
				}
				if err != nil {
					break
				}
			}

			var contentBlocks []AnthropicContentBlock
			if fullText.Len() > 0 {
				contentBlocks = append(contentBlocks, AnthropicContentBlock{
					Type: "text",
					Text: fullText.String(),
				})
			}
			var tcKeys []int
			for k := range toolCallsMap {
				tcKeys = append(tcKeys, k)
			}
			sort.Ints(tcKeys)
			for _, k := range tcKeys {
				tc := toolCallsMap[k]
				var parsedArgs map[string]interface{}
				if argsStr, ok := tc["args"].(string); ok {
					json.Unmarshal([]byte(argsStr), &parsedArgs)
				}
				if parsedArgs == nil {
					parsedArgs = make(map[string]interface{})
				}
				tcID, _ := tc["id"].(string)
				tcName, _ := tc["name"].(string)
				contentBlocks = append(contentBlocks, AnthropicContentBlock{
					Type:  "tool_use",
					ID:    tcID,
					Name:  tcName,
					Input: parsedArgs,
				})
			}
			if len(contentBlocks) == 0 {
				contentBlocks = append(contentBlocks, AnthropicContentBlock{Type: "text", Text: ""})
			}

			antResp := AnthropicMessageResponse{
				ID:           msgID,
				Type:         "message",
				Role:         "assistant",
				Content:      contentBlocks,
				Model:        req.Model,
				StopReason:   stopReason,
				StopSequence: nil,
				Usage: AnthropicUsage{
					InputTokens:  100,
					OutputTokens: fullText.Len() / 4,
				},
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			json.NewEncoder(w).Encode(antResp)
			return
		}
	}

	// Streaming SSE Response (Anthropic Format)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, isFlusher := w.(http.Flusher)
	sendSSE := func(eventType string, data interface{}) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(b))
		if isFlusher {
			flusher.Flush()
		}
	}

	// 1. message_start
	sendSSE("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         req.Model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	})

	scanner := bufio.NewScanner(upResp.Body)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)

	var textBlockStarted bool
	var activeToolIndex = -1
	var currentToolID string
	var currentToolName string
	var currentBlockIndex = 0
	stopReason := "end_turn"

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		dataStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if dataStr == "[DONE]" || dataStr == "" {
			continue
		}

		var chunk struct {
			Type  string `json:"type"`
			Delta interface{} `json:"delta"`
			Item  map[string]interface{} `json:"item"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}

		if err := json.Unmarshal([]byte(dataStr), &chunk); err != nil {
			continue
		}

		// Handle /responses SSE format in streaming mode
		if isResponses {
			if chunk.Type == "response.output_text.delta" {
				if deltaStr, ok := chunk.Delta.(string); ok {
					if !textBlockStarted {
						sendSSE("content_block_start", map[string]interface{}{
							"type":         "content_block_start",
							"index":        currentBlockIndex,
							"content_block": map[string]interface{}{"type": "text", "text": ""},
						})
						textBlockStarted = true
					}
					sendSSE("content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": currentBlockIndex,
						"delta": map[string]interface{}{"type": "text_delta", "text": deltaStr},
					})
				}
			} else if chunk.Type == "response.output_item.added" {
				if chunk.Item != nil && chunk.Item["type"] == "function_call" {
					if textBlockStarted {
						sendSSE("content_block_stop", map[string]interface{}{
							"type":  "content_block_stop",
							"index": currentBlockIndex,
						})
						textBlockStarted = false
						currentBlockIndex++
					}
					stopReason = "tool_use"
					callID, _ := chunk.Item["call_id"].(string)
					fnName, _ := chunk.Item["name"].(string)
					sendSSE("content_block_start", map[string]interface{}{
						"type":  "content_block_start",
						"index": currentBlockIndex,
						"content_block": map[string]interface{}{
							"type":  "tool_use",
							"id":    callID,
							"name":  fnName,
							"input": map[string]interface{}{},
						},
					})
					activeToolIndex = 0
				}
			} else if chunk.Type == "response.function_call_arguments.delta" {
				if deltaArgs, ok := chunk.Delta.(string); ok {
					sendSSE("content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": currentBlockIndex,
						"delta": map[string]interface{}{
							"type":         "input_json_delta",
							"partial_json": deltaArgs,
						},
					})
				}
			}
			continue
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]

		if choice.FinishReason == "tool_calls" {
			stopReason = "tool_use"
		} else if choice.FinishReason == "length" {
			stopReason = "max_tokens"
		}

		// Text delta
		if choice.Delta.Content != "" {
			if !textBlockStarted {
				sendSSE("content_block_start", map[string]interface{}{
					"type":         "content_block_start",
					"index":        currentBlockIndex,
					"content_block": map[string]interface{}{"type": "text", "text": ""},
				})
				textBlockStarted = true
			}
			sendSSE("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": currentBlockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": choice.Delta.Content},
			})
		}

		// Tool calls streaming
		for _, tc := range choice.Delta.ToolCalls {
			if tc.ID != "" || tc.Function.Name != "" {
				if textBlockStarted {
					sendSSE("content_block_stop", map[string]interface{}{
						"type":  "content_block_stop",
						"index": currentBlockIndex,
					})
					textBlockStarted = false
					currentBlockIndex++
				}
				if activeToolIndex >= 0 {
					sendSSE("content_block_stop", map[string]interface{}{
						"type":  "content_block_stop",
						"index": currentBlockIndex,
					})
					currentBlockIndex++
				}
				activeToolIndex = tc.Index
				currentToolID = tc.ID
				currentToolName = tc.Function.Name
				sendSSE("content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": currentBlockIndex,
					"content_block": map[string]interface{}{
						"type":  "tool_use",
						"id":    currentToolID,
						"name":  currentToolName,
						"input": map[string]interface{}{},
					},
				})
			}

			if tc.Function.Arguments != "" {
				sendSSE("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": currentBlockIndex,
					"delta": map[string]interface{}{
						"type":         "input_json_delta",
						"partial_json": tc.Function.Arguments,
					},
				})
			}
		}
	}

	if textBlockStarted || activeToolIndex >= 0 {
		sendSSE("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": currentBlockIndex,
		})
	}

	sendSSE("message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]interface{}{
			"output_tokens": 0,
		},
	})

	sendSSE("message_stop", map[string]interface{}{
		"type": "message_stop",
	})
}
