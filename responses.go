package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func sendResponsesError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"message": msg,
		},
	})
}

func parseResponsesContent(content interface{}) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	if blocks, ok := content.([]interface{}); ok {
		var sb strings.Builder
		for _, b := range blocks {
			if m, ok := b.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok && t != "" {
					sb.WriteString(t)
				} else if val, ok := m["text"].(map[string]interface{}); ok {
					if t2, ok := val["value"].(string); ok && t2 != "" {
						sb.WriteString(t2)
					}
				}
			}
		}
		return sb.String()
	}
	return ""
}

func parseResponsesMessageContent(content interface{}) interface{} {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	if blocks, ok := content.([]interface{}); ok {
		hasMedia := false
		var outBlocks []map[string]interface{}
		var textParts []string

		for _, b := range blocks {
			if m, ok := b.(map[string]interface{}); ok {
				bType, _ := m["type"].(string)
				if bType == "input_image" || bType == "image_url" || bType == "image" {
					hasMedia = true
					imgURL := ""
					if u, ok := m["image_url"].(string); ok {
						imgURL = u
					} else if uMap, ok := m["image_url"].(map[string]interface{}); ok {
						if u2, ok := uMap["url"].(string); ok {
							imgURL = u2
						}
					} else if u, ok := m["url"].(string); ok {
						imgURL = u
					} else if src, ok := m["source"].(map[string]interface{}); ok {
						mt, _ := src["media_type"].(string)
						d, _ := src["data"].(string)
						if mt != "" && d != "" {
							imgURL = fmt.Sprintf("data:%s;base64,%s", mt, d)
						}
					}
					if imgURL != "" {
						outBlocks = append(outBlocks, map[string]interface{}{
							"type": "image_url",
							"image_url": map[string]interface{}{
								"url": imgURL,
							},
						})
					}
				} else if bType == "input_text" || bType == "text" || bType == "output_text" {
					txt := ""
					if t, ok := m["text"].(string); ok {
						txt = t
					} else if val, ok := m["text"].(map[string]interface{}); ok {
						if t2, ok := val["value"].(string); ok {
							txt = t2
						}
					}
					if txt != "" {
						textParts = append(textParts, txt)
						outBlocks = append(outBlocks, map[string]interface{}{
							"type": "text",
							"text": txt,
						})
					}
				}
			}
		}
		if hasMedia {
			return outBlocks
		}
		if len(textParts) > 0 {
			return strings.Join(textParts, "\n\n")
		}
	}
	return ""
}

func handleResponses(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")

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

	if r.Method != "POST" {
		sendResponsesError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Only POST method is supported")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	var reqBody map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		recordReq(400, "unknown", time.Since(start).Milliseconds())
		sendResponsesError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}

	rawModel, _ := reqBody["model"].(string)
	model := resolveModelName(rawModel)
	isStream, _ := reqBody["stream"].(bool)

	// Auth token & BYOK resolution
	okAuth, auth := resolveAuth(r)
	if !okAuth {
		recordReq(401, model, time.Since(start).Milliseconds())
		sendResponsesError(w, http.StatusUnauthorized, "invalid_request_error", "invalid proxy key")
		return
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

	// Extract top-level instructions if present
	var userInstructions []string
	if inst, ok := reqBody["instructions"].(string); ok && strings.TrimSpace(inst) != "" {
		userInstructions = append(userInstructions, strings.TrimSpace(inst))
	}

	// Normalize input items
	rawInput := reqBody["input"]
	var normalizedItems []map[string]interface{}

	if inputStr, ok := rawInput.(string); ok {
		normalizedItems = append(normalizedItems, map[string]interface{}{
			"role":    "user",
			"content": inputStr,
		})
	} else if inputList, ok := rawInput.([]interface{}); ok {
		for _, item := range inputList {
			if im, ok := item.(map[string]interface{}); ok {
				iType, _ := im["type"].(string)
				role, _ := im["role"].(string)

				if iType == "function_call" || iType == "function_call_output" || iType == "reasoning" {
					normalizedItems = append(normalizedItems, im)
				} else if role != "" {
					normalizedItems = append(normalizedItems, im)
				} else if iType == "message" {
					normalizedItems = append(normalizedItems, im)
				} else {
					normalizedItems = append(normalizedItems, im)
				}
			}
		}
	}

	isResponses := modelFormat(model) == "responses"
	targetURL := strings.TrimRight(upstreamBase, "/") + "/chat/completions"
	if isResponses {
		targetURL = strings.TrimRight(upstreamBase, "/") + "/responses"
	}

	var upstreamPayload map[string]interface{}

	if isResponses {
		// Prepare upstream payload for /responses (muse-* etc.)
		var inputItems []map[string]interface{}

		for _, item := range normalizedItems {
			role, _ := item["role"].(string)
			if role == "system" || role == "developer" {
				txt := parseResponsesContent(item["content"])
				if txt != "" {
					userInstructions = append(userInstructions, txt)
				}
			}
		}

		combSys := sysPromptMuse
		if len(userInstructions) > 0 {
			combSys += "\n\n# User Instructions & Guidelines:\n" + strings.Join(userInstructions, "\n")
		}
		inputItems = append(inputItems, map[string]interface{}{"role": "developer", "content": combSys})

		for _, item := range normalizedItems {
			role, _ := item["role"].(string)
			iType, _ := item["type"].(string)

			if role == "system" || role == "developer" {
				continue
			}

			if iType == "function_call" {
				inputItems = append(inputItems, item)
			} else if iType == "function_call_output" {
				inputItems = append(inputItems, item)
			} else if role == "tool" {
				callID, _ := item["tool_call_id"].(string)
				if callID == "" {
					callID, _ = item["call_id"].(string)
				}
				content := parseResponsesContent(item["content"])
				inputItems = append(inputItems, map[string]interface{}{
					"type":    "function_call_output",
					"call_id": callID,
					"output":  content,
				})
			} else if role == "assistant" {
				content := parseResponsesContent(item["content"])
				if content != "" {
					inputItems = append(inputItems, map[string]interface{}{"role": "assistant", "content": content})
				}
				if tcs, ok := item["tool_calls"].([]interface{}); ok {
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
			} else if role == "user" {
				content := parseResponsesMessageContent(item["content"])
				inputItems = append(inputItems, map[string]interface{}{"role": "user", "content": content})
			} else if iType == "reasoning" {
				inputItems = append(inputItems, item)
			} else {
				inputItems = append(inputItems, item)
			}
		}

		// Merge tools
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
					// Handle flat Responses format or nested Chat format
					if fn, ok := tm["function"].(map[string]interface{}); ok {
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
					} else if fnName, ok := tm["name"].(string); ok {
						if !existingNames[fnName] {
							mergedTools = append(mergedTools, map[string]interface{}{
								"type":        "function",
								"name":        fnName,
								"description": tm["description"],
								"parameters":  tm["parameters"],
								"strict":      false,
							})
							existingNames[fnName] = true
						}
					}
				}
			}
		}

		maxOutputTokens := 32000
		if mot, ok := reqBody["max_output_tokens"].(float64); ok && int(mot) > 0 {
			maxOutputTokens = int(mot)
		} else if mt, ok := reqBody["max_tokens"].(float64); ok && int(mt) > 0 {
			maxOutputTokens = int(mt)
		}

		upstreamPayload = map[string]interface{}{
			"model":              model,
			"input":              inputItems,
			"tools":              mergedTools,
			"tool_choice":        "auto",
			"stream":             true,
			"max_output_tokens": maxOutputTokens,
			"store":              false,
			"include":            []string{"reasoning.encrypted_content"},
			"prompt_cache_key":   sessionID,
		}
	} else {
		// Upstream model is Chat (mimo-*, nemotron-*, space-bunny-*, etc.)
		var upMsgs []interface{}

		for _, item := range normalizedItems {
			role, _ := item["role"].(string)
			if role == "system" || role == "developer" {
				txt := parseResponsesContent(item["content"])
				if txt != "" {
					userInstructions = append(userInstructions, txt)
				}
			}
		}

		combSys := sysPromptChat
		if len(userInstructions) > 0 {
			combSys += "\n\n# User Instructions & Guidelines:\n" + strings.Join(userInstructions, "\n")
		}
		upMsgs = append(upMsgs, map[string]interface{}{"role": "system", "content": combSys})

		for _, item := range normalizedItems {
			role, _ := item["role"].(string)
			iType, _ := item["type"].(string)

			if role == "system" || role == "developer" {
				continue
			}

			if iType == "function_call" {
				callID, _ := item["call_id"].(string)
				name, _ := item["name"].(string)
				args, _ := item["arguments"].(string)
				upMsgs = append(upMsgs, map[string]interface{}{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []map[string]interface{}{
						{
							"id":   callID,
							"type": "function",
							"function": map[string]interface{}{
								"name":      name,
								"arguments": args,
							},
						},
					},
				})
			} else if iType == "function_call_output" {
				callID, _ := item["call_id"].(string)
				output, _ := item["output"].(string)
				upMsgs = append(upMsgs, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": callID,
					"content":      output,
				})
			} else if role == "tool" {
				callID, _ := item["tool_call_id"].(string)
				if callID == "" {
					callID, _ = item["call_id"].(string)
				}
				content := parseResponsesContent(item["content"])
				upMsgs = append(upMsgs, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": callID,
					"content":      content,
				})
			} else if role == "assistant" {
				content := parseResponsesContent(item["content"])
				msgMap := map[string]interface{}{"role": "assistant", "content": content}
				if tcs, ok := item["tool_calls"].([]interface{}); ok && len(tcs) > 0 {
					msgMap["tool_calls"] = tcs
				}
				upMsgs = append(upMsgs, msgMap)
			} else if role == "user" {
				content := parseResponsesMessageContent(item["content"])
				upMsgs = append(upMsgs, map[string]interface{}{"role": "user", "content": content})
			}
		}

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
					} else if fnName, ok := tm["name"].(string); ok {
						if !existingNames[fnName] {
							mergedTools = append(mergedTools, map[string]interface{}{
								"type": "function",
								"function": map[string]interface{}{
									"name":        fnName,
									"description": tm["description"],
									"parameters":  tm["parameters"],
								},
							})
							existingNames[fnName] = true
						}
					}
				}
			}
		}

		maxTokens := 32000
		if mt, ok := reqBody["max_output_tokens"].(float64); ok && int(mt) > 0 {
			maxTokens = int(mt)
		} else if mt2, ok := reqBody["max_tokens"].(float64); ok && int(mt2) > 0 {
			maxTokens = int(mt2)
		}

		upstreamPayload = map[string]interface{}{
			"model":      model,
			"messages":   upMsgs,
			"tools":      mergedTools,
			"stream":     true,
			"max_tokens": maxTokens,
		}
	}

	bodyBytes, _ := json.Marshal(upstreamPayload)
	upReq, err := http.NewRequest("POST", targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		recordReq(500, model, time.Since(start).Milliseconds())
		sendResponsesError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	upReq.Header = headers

	client := getUpstreamClient(time.Duration(timeoutSec) * time.Second)
	upResp, err := client.Do(upReq)
	if err != nil {
		recordReq(502, model, time.Since(start).Milliseconds())
		sendResponsesError(w, http.StatusBadGateway, "upstream_error", fmt.Sprintf("upstream error: %v", err))
		return
	}
	defer upResp.Body.Close()

	if upResp.StatusCode != 200 {
		b, _ := io.ReadAll(upResp.Body)
		recordReq(upResp.StatusCode, model, time.Since(start).Milliseconds())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upResp.StatusCode)
		w.Write(b)
		return
	}

	recordReq(200, model, time.Since(start).Milliseconds())
	randHex := make([]byte, 8)
	rand.Read(randHex)
	respID := "resp_" + hex.EncodeToString(randHex)
	msgID := "msg_" + hex.EncodeToString(randHex)
	createdTime := time.Now().Unix()

	if isResponses {
		// UPSTREAM IS /responses (SSE Stream from muse-*)
		reader := bufio.NewReader(upResp.Body)

		if isStream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			flusher, _ := w.(http.Flusher)

			for {
				line, err := reader.ReadBytes('\n')
				if len(line) > 0 {
					w.Write(line)
					if flusher != nil {
						flusher.Flush()
					}
				}
				if err != nil {
					break
				}
			}
			return
		}

		// Non-Streaming Response for /responses: accumulate events
		var fullText strings.Builder
		var functionCalls []map[string]interface{}
		var finalResponse map[string]interface{}

		for {
			line, err := reader.ReadString('\n')
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data: ") && trimmed != "data: [DONE]" {
				var event map[string]interface{}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(trimmed, "data: ")), &event); err == nil {
					eType, _ := event["type"].(string)
					if eType == "response.output_text.delta" {
						if delta, ok := event["delta"].(string); ok {
							fullText.WriteString(delta)
						}
					} else if eType == "response.output_item.added" {
						if item, ok := event["item"].(map[string]interface{}); ok {
							if item["type"] == "function_call" {
								functionCalls = append(functionCalls, map[string]interface{}{
									"type":      "function_call",
									"id":        item["id"],
									"call_id":   item["call_id"],
									"name":      item["name"],
									"arguments": "",
									"status":    "completed",
								})
							}
						}
					} else if eType == "response.function_call_arguments.delta" {
						if delta, ok := event["delta"].(string); ok && len(functionCalls) > 0 {
							lastIdx := len(functionCalls) - 1
							curArgs, _ := functionCalls[lastIdx]["arguments"].(string)
							functionCalls[lastIdx]["arguments"] = curArgs + delta
						}
					} else if eType == "response.completed" {
						if respObj, ok := event["response"].(map[string]interface{}); ok {
							finalResponse = respObj
						}
						break
					}
				}
			}
			if err != nil {
				break
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if finalResponse != nil {
			json.NewEncoder(w).Encode(finalResponse)
			return
		}

		// Fallback synthesizer if response.completed wasn't caught
		var outputItems []map[string]interface{}
		if fullText.Len() > 0 {
			outputItems = append(outputItems, map[string]interface{}{
				"id":     msgID,
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []map[string]interface{}{
					{
						"type": "output_text",
						"text": fullText.String(),
					},
				},
			})
		}
		for _, fc := range functionCalls {
			outputItems = append(outputItems, fc)
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":         respID,
			"object":     "response",
			"created_at": createdTime,
			"model":      model,
			"status":     "completed",
			"output":     outputItems,
			"usage": map[string]interface{}{
				"total_tokens":  0,
				"input_tokens":  0,
				"output_tokens": 0,
			},
		})
		return
	}

	// UPSTREAM IS /chat/completions (SSE Stream from mimo-*, nemotron-*, etc.)
	reader := bufio.NewReader(upResp.Body)

	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)

		// 1. Emit response.created
		seq := 0
		initResp := map[string]interface{}{
			"id":         respID,
			"object":     "response",
			"created_at": createdTime,
			"status":     "in_progress",
			"model":      model,
			"output":     []interface{}{},
			"usage":      nil,
		}
		evCreated, _ := json.Marshal(map[string]interface{}{
			"type":            "response.created",
			"sequence_number": seq,
			"response":        initResp,
		})
		fmt.Fprintf(w, "event: response.created\ndata: %s\n\n", string(evCreated))
		seq++

		evInProg, _ := json.Marshal(map[string]interface{}{
			"type":            "response.in_progress",
			"sequence_number": seq,
			"response":        initResp,
		})
		fmt.Fprintf(w, "event: response.in_progress\ndata: %s\n\n", string(evInProg))
		seq++
		if flusher != nil {
			flusher.Flush()
		}

		messageStarted := false
		outputIndex := 0
		var fullText strings.Builder
		toolCallsMap := make(map[int]map[string]interface{})

		for {
			line, err := reader.ReadString('\n')
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data: ") {
				payload := strings.TrimPrefix(trimmed, "data: ")
				if payload == "[DONE]" {
					break
				}
				var chunk map[string]interface{}
				if err := json.Unmarshal([]byte(payload), &chunk); err == nil {
					choices, _ := chunk["choices"].([]interface{})
					if len(choices) > 0 {
						choice, _ := choices[0].(map[string]interface{})
						delta, _ := choice["delta"].(map[string]interface{})

						// Text content delta
						if content, ok := delta["content"].(string); ok && content != "" {
							if !messageStarted {
								messageStarted = true
								// emit response.output_item.added
								itemAdded, _ := json.Marshal(map[string]interface{}{
									"type":            "response.output_item.added",
									"sequence_number": seq,
									"output_index":    outputIndex,
									"item": map[string]interface{}{
										"id":      msgID,
										"type":    "message",
										"status":  "in_progress",
										"role":    "assistant",
										"content": []interface{}{},
									},
								})
								fmt.Fprintf(w, "event: response.output_item.added\ndata: %s\n\n", string(itemAdded))
								seq++

								// emit response.content_part.added
								partAdded, _ := json.Marshal(map[string]interface{}{
									"type":            "response.content_part.added",
									"sequence_number": seq,
									"output_index":    outputIndex,
									"content_index":   0,
									"item_id":         msgID,
									"part": map[string]interface{}{
										"type":        "output_text",
										"text":        "",
										"annotations": []interface{}{},
									},
								})
								fmt.Fprintf(w, "event: response.content_part.added\ndata: %s\n\n", string(partAdded))
								seq++
							}

							fullText.WriteString(content)
							textDelta, _ := json.Marshal(map[string]interface{}{
								"type":            "response.output_text.delta",
								"sequence_number": seq,
								"output_index":    outputIndex,
								"content_index":   0,
								"item_id":         msgID,
								"delta":           content,
							})
							fmt.Fprintf(w, "event: response.output_text.delta\ndata: %s\n\n", string(textDelta))
							seq++
							if flusher != nil {
								flusher.Flush()
							}
						}

						// Tool calls delta
						if tcs, ok := delta["tool_calls"].([]interface{}); ok {
							for _, tcItem := range tcs {
								tc, ok := tcItem.(map[string]interface{})
								if !ok {
									continue
								}
								idx := 0
								if fidx, ok := tc["index"].(float64); ok {
									idx = int(fidx)
								}
								entry, exists := toolCallsMap[idx]
								if !exists {
									outputIndex++
									callID, _ := tc["id"].(string)
									if callID == "" {
										callID = fmt.Sprintf("call_%s_%d", respID[5:], idx)
									}
									fn, _ := tc["function"].(map[string]interface{})
									fnName, _ := fn["name"].(string)
									entry = map[string]interface{}{
										"type":         "function_call",
										"id":           fmt.Sprintf("item_%s_%d", respID[5:], idx),
										"call_id":      callID,
										"name":         fnName,
										"arguments":    "",
										"output_index": outputIndex,
									}
									toolCallsMap[idx] = entry

									itemAdded, _ := json.Marshal(map[string]interface{}{
										"type":            "response.output_item.added",
										"sequence_number": seq,
										"output_index":    outputIndex,
										"item": map[string]interface{}{
											"id":        entry["id"],
											"type":      "function_call",
											"call_id":   callID,
											"name":      fnName,
											"arguments": "",
											"status":    "in_progress",
										},
									})
									fmt.Fprintf(w, "event: response.output_item.added\ndata: %s\n\n", string(itemAdded))
									seq++
								}

								if fn, ok := tc["function"].(map[string]interface{}); ok {
									if argDelta, ok := fn["arguments"].(string); ok && argDelta != "" {
										curArgs, _ := entry["arguments"].(string)
										entry["arguments"] = curArgs + argDelta
										outIdx := entry["output_index"].(int)
										callID := entry["call_id"].(string)
										itemID := entry["id"].(string)

										argEv, _ := json.Marshal(map[string]interface{}{
											"type":            "response.function_call_arguments.delta",
											"sequence_number": seq,
											"output_index":    outIdx,
											"item_id":         itemID,
											"call_id":         callID,
											"delta":           argDelta,
										})
										fmt.Fprintf(w, "event: response.function_call_arguments.delta\ndata: %s\n\n", string(argEv))
										seq++
										if flusher != nil {
											flusher.Flush()
										}
									}
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

		var finalOutput []map[string]interface{}
		if messageStarted {
			partDone, _ := json.Marshal(map[string]interface{}{
				"type":            "response.content_part.done",
				"sequence_number": seq,
				"output_index":    0,
				"content_index":   0,
				"item_id":         msgID,
				"part": map[string]interface{}{
					"type":        "output_text",
					"text":        fullText.String(),
					"annotations": []interface{}{},
				},
			})
			fmt.Fprintf(w, "event: response.content_part.done\ndata: %s\n\n", string(partDone))
			seq++

			msgItemDone, _ := json.Marshal(map[string]interface{}{
				"type":            "response.output_item.done",
				"sequence_number": seq,
				"output_index":    0,
				"item": map[string]interface{}{
					"id":     msgID,
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]interface{}{
						{
							"type":        "output_text",
							"text":        fullText.String(),
							"annotations": []interface{}{},
						},
					},
				},
			})
			fmt.Fprintf(w, "event: response.output_item.done\ndata: %s\n\n", string(msgItemDone))
			seq++

			finalOutput = append(finalOutput, map[string]interface{}{
				"id":     msgID,
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []map[string]interface{}{
					{
						"type":        "output_text",
						"text":        fullText.String(),
						"annotations": []interface{}{},
					},
				},
			})
		}

		for _, entry := range toolCallsMap {
			outIdx := entry["output_index"].(int)
			callID := entry["call_id"].(string)
			args := entry["arguments"].(string)
			fnName := entry["name"].(string)
			itemID := entry["id"].(string)

			fnDone, _ := json.Marshal(map[string]interface{}{
				"type":            "response.function_call_arguments.done",
				"sequence_number": seq,
				"output_index":    outIdx,
				"item_id":         itemID,
				"call_id":         callID,
				"arguments":       args,
			})
			fmt.Fprintf(w, "event: response.function_call_arguments.done\ndata: %s\n\n", string(fnDone))
			seq++

			fcDone, _ := json.Marshal(map[string]interface{}{
				"type":            "response.output_item.done",
				"sequence_number": seq,
				"output_index":    outIdx,
				"item": map[string]interface{}{
					"id":        itemID,
					"type":      "function_call",
					"call_id":   callID,
					"name":      fnName,
					"arguments": args,
					"status":    "completed",
				},
			})
			fmt.Fprintf(w, "event: response.output_item.done\ndata: %s\n\n", string(fcDone))
			seq++

			finalOutput = append(finalOutput, map[string]interface{}{
				"id":        itemID,
				"type":      "function_call",
				"call_id":   callID,
				"name":      fnName,
				"arguments": args,
				"status":    "completed",
			})
		}

		tokenCount := len(strings.Fields(fullText.String()))
		completedEv, _ := json.Marshal(map[string]interface{}{
			"type":            "response.completed",
			"sequence_number": seq,
			"response": map[string]interface{}{
				"id":           respID,
				"object":       "response",
				"created_at":   createdTime,
				"completed_at": time.Now().Unix(),
				"model":        model,
				"status":       "completed",
				"output":       finalOutput,
				"usage": map[string]interface{}{
					"total_tokens":  tokenCount + 10,
					"input_tokens":  10,
					"output_tokens": tokenCount,
				},
			},
		})
		fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", string(completedEv))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	// Non-Streaming Response for /chat/completions upstream: accumulate chunks
	var fullText strings.Builder
	toolCallsMap := make(map[int]map[string]interface{})

	for {
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data: ") {
			payload := strings.TrimPrefix(trimmed, "data: ")
			if payload == "[DONE]" {
				break
			}
			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(payload), &chunk); err == nil {
				choices, _ := chunk["choices"].([]interface{})
				if len(choices) > 0 {
					choice, _ := choices[0].(map[string]interface{})
					delta, _ := choice["delta"].(map[string]interface{})
					if content, ok := delta["content"].(string); ok {
						fullText.WriteString(content)
					}
					if tcs, ok := delta["tool_calls"].([]interface{}); ok {
						for _, tcItem := range tcs {
							if tc, ok := tcItem.(map[string]interface{}); ok {
								idx := 0
								if fidx, ok := tc["index"].(float64); ok {
									idx = int(fidx)
								}
								entry, exists := toolCallsMap[idx]
								if !exists {
									callID, _ := tc["id"].(string)
									if callID == "" {
										callID = fmt.Sprintf("call_%s_%d", respID[5:], idx)
									}
									fn, _ := tc["function"].(map[string]interface{})
									fnName, _ := fn["name"].(string)
									entry = map[string]interface{}{
										"type":      "function_call",
										"id":        fmt.Sprintf("item_%s_%d", respID[5:], idx),
										"call_id":   callID,
										"name":      fnName,
										"arguments": "",
									}
									toolCallsMap[idx] = entry
								}
								if fn, ok := tc["function"].(map[string]interface{}); ok {
									if argDelta, ok := fn["arguments"].(string); ok {
										curArgs, _ := entry["arguments"].(string)
										entry["arguments"] = curArgs + argDelta
									}
								}
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

	var outputItems []map[string]interface{}
	if fullText.Len() > 0 {
		outputItems = append(outputItems, map[string]interface{}{
			"id":     msgID,
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []map[string]interface{}{
				{
					"type": "output_text",
					"text": fullText.String(),
				},
			},
		})
	}
	for _, entry := range toolCallsMap {
		outputItems = append(outputItems, map[string]interface{}{
			"id":        entry["id"],
			"type":      "function_call",
			"call_id":   entry["call_id"],
			"name":      entry["name"],
			"arguments": entry["arguments"],
			"status":    "completed",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":         respID,
		"object":     "response",
		"created_at": createdTime,
		"model":      model,
		"status":     "completed",
		"output":     outputItems,
		"usage": map[string]interface{}{
			"total_tokens":  0,
			"input_tokens":  0,
			"output_tokens": 0,
		},
	})
}
