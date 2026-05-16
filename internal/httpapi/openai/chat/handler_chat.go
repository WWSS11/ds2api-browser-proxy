package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ds2api/internal/assistantturn"
	"ds2api/internal/auth"
	"ds2api/internal/browserproxy"
	"ds2api/internal/completionruntime"
	"ds2api/internal/config"
	dsprotocol "ds2api/internal/deepseek/protocol"
	openaifmt "ds2api/internal/format/openai"
	"ds2api/internal/promptcompat"
	"ds2api/internal/sse"
	streamengine "ds2api/internal/stream"
)

func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	if isVercelStreamReleaseRequest(r) {
		h.handleVercelStreamRelease(w, r)
		return
	}
	if isVercelStreamPowRequest(r) {
		h.handleVercelStreamPow(w, r)
		return
	}
	if isVercelStreamSwitchRequest(r) {
		h.handleVercelStreamSwitch(w, r)
		return
	}
	if isVercelStreamPrepareRequest(r) {
		h.handleVercelStreamPrepare(w, r)
		return
	}

	var a *auth.RequestAuth
	var sessionID string
	var reusedSession bool
	if h.Store.BrowserProxyEnabled() {
		config.Logger.Info("[browser_proxy] using simplified auth (browser-only mode)")
		authHeader := r.Header.Get("Authorization")
		callerKey := ""
		if strings.HasPrefix(authHeader, "Bearer ") {
			callerKey = strings.TrimPrefix(authHeader, "Bearer ")
		}
		if callerKey == "" {
			writeOpenAIError(w, http.StatusUnauthorized, "missing Authorization header")
			return
		}
		a = &auth.RequestAuth{
			CallerID:  callerKey,
			AccountID: "browser-proxy",
		}
		defer func() {}()
	} else {
		var err error
		a, err = h.Auth.Determine(r)
		if err != nil {
			status := http.StatusUnauthorized
			detail := err.Error()
			if err == auth.ErrNoAccount {
				status = http.StatusTooManyRequests
			}
			writeOpenAIError(w, status, detail)
			return
		}
		defer func() {
			if !reusedSession {
				h.autoDeleteRemoteSession(r.Context(), a, sessionID)
			}
			h.Auth.Release(a)
		}()
	}

	r = r.WithContext(auth.WithAuth(r.Context(), a))

	r.Body = http.MaxBytesReader(w, r.Body, openAIGeneralMaxSize)
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "too large") {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if msgs, ok := req["messages"].([]any); ok && len(msgs) > 0 {
		if m, ok := msgs[len(msgs)-1].(map[string]any); ok {
			if c, ok := m["content"].(string); ok {
				config.Logger.Info("[chat_handler] raw content check",
					"content_len", len(c),
					"content_preview", c[:min(50, len(c))],
					"content_bytes", fmt.Sprintf("%x", []byte(c[:min(20, len(c))])),
				)
			}
		}
	}
	if err := h.preprocessInlineFileInputs(r.Context(), a, req); err != nil {
		writeOpenAIInlineFileError(w, err)
		return
	}
	stdReq, err := promptcompat.NormalizeOpenAIChatRequest(h.Store, req, requestTraceID(r))
	if err != nil {
		config.Logger.Error("[chat_handler] normalize error", "error", err)
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	config.Logger.Info("[chat_handler] request normalized", "model", stdReq.ResponseModel, "stream", stdReq.Stream, "prompt_len", len(stdReq.PromptTokenText), "msg_count", len(stdReq.Messages), "has_tools", stdReq.ToolsRaw != nil)
	const maxPromptChars = 1000000
	if len(stdReq.PromptTokenText) > maxPromptChars {
		config.Logger.Warn("[chat_handler] prompt too long, rejecting", "prompt_len", len(stdReq.PromptTokenText), "max", maxPromptChars)
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("prompt too long (%d chars, max %d). Reduce message count or tool result size.", len(stdReq.PromptTokenText), maxPromptChars))
		return
	}
	stdReq, err = h.applyCurrentInputFile(r.Context(), a, stdReq)
	if err != nil {
		status, message := mapCurrentInputFileError(err)
		writeOpenAIError(w, status, message)
		return
	}

	config.Logger.Info("[browser_proxy_check] checking",
		"browser_session_not_nil", h.BrowserSession != nil,
		"browser_proxy_enabled", h.Store.BrowserProxyEnabled(),
		"model", stdReq.ResponseModel,
	)

	if shouldUseBrowserProxy(stdReq.ResponseModel, h.Store.BrowserProxyEnabled()) {
		session := h.BrowserSession
		if session == nil {
			config.Logger.Info("[browser_proxy] session nil, attempting lazy init")
			cfg := browserproxy.NewConfig(h.Store.BrowserProxy())
			accounts := h.Store.Accounts()
			if len(accounts) == 0 {
				writeOpenAIError(w, http.StatusServiceUnavailable, "no accounts configured")
				return
			}
			session = browserproxy.NewBrowserSession(cfg, accounts[0])
			if err := session.Start(r.Context()); err != nil {
				config.Logger.Error("[browser_proxy] lazy init failed", "error", err)
				writeOpenAIError(w, http.StatusServiceUnavailable, fmt.Sprintf("browser init failed: %v", err))
				return
			}
			h.BrowserSession = session
			config.Logger.Info("[browser_proxy] lazy init success")
		}

		config.Logger.Info("[browser_proxy] using browser-only mode (safe mode), skipping Trust.Login")
		h.handleBrowserProxyChat(w, r, a, stdReq)
		return
	}

	config.Logger.Warn("[direct_api] using direct API mode",
		"model", stdReq.ResponseModel,
		"warning", "direct API may be detected and blocked by official",
		"has_tools", stdReq.ToolsRaw != nil,
	)

	historySession := startChatHistory(h.ChatHistory, r, a, stdReq)

	if !stdReq.Stream {
		result, outErr := completionruntime.ExecuteNonStreamWithRetry(r.Context(), h.DS, a, stdReq, completionruntime.Options{
			RetryEnabled:     true,
			CurrentInputFile: h.Store,
		})
		sessionID = result.SessionID
		if outErr != nil {
			if historySession != nil {
				historySession.error(outErr.Status, outErr.Message, outErr.Code, historyThinkingForArchive(result.Turn.RawThinking, result.Turn.DetectionThinking, result.Turn.Thinking), historyTextForArchive(result.Turn.RawText, result.Turn.Text))
			}
			writeOpenAIErrorWithCode(w, outErr.Status, outErr.Message, outErr.Code)
			return
		}
		respBody := openaifmt.BuildChatCompletionWithToolCalls(result.SessionID, stdReq.ResponseModel, result.Turn.Prompt, result.Turn.Thinking, result.Turn.Text, result.Turn.ToolCalls, stdReq.ToolsRaw)
		respBody["usage"] = assistantturn.OpenAIChatUsage(result.Turn)
		finishReason := assistantturn.FinalizeTurn(result.Turn, assistantturn.FinalizeOptions{}).FinishReason
		if historySession != nil {
			historySession.success(http.StatusOK, historyThinkingForArchive(result.Turn.RawThinking, result.Turn.DetectionThinking, result.Turn.Thinking), historyTextForArchive(result.Turn.RawText, result.Turn.Text), finishReason, assistantturn.OpenAIChatUsage(result.Turn))
		}
		writeJSON(w, http.StatusOK, respBody)
		return
	}

	start, outErr := completionruntime.StartCompletion(r.Context(), h.DS, a, stdReq, completionruntime.Options{
		CurrentInputFile: h.Store,
	})
	sessionID = start.SessionID
	if outErr != nil {
		if historySession != nil {
			historySession.error(outErr.Status, outErr.Message, outErr.Code, "", "")
		}
		writeOpenAIErrorWithCode(w, outErr.Status, outErr.Message, outErr.Code)
		return
	}
	streamReq := start.Request
	refFileTokens := streamReq.RefFileTokens
	h.handleStreamWithRetry(w, r, a, start.Response, start.Payload, start.Pow, sessionID, &sessionID, streamReq, streamReq.ResponseModel, streamReq.PromptTokenText, refFileTokens, streamReq.Thinking, streamReq.Search, streamReq.ToolNames, streamReq.ToolsRaw, streamReq.ToolChoice, historySession)
}

func (h *Handler) autoDeleteRemoteSession(ctx context.Context, a *auth.RequestAuth, sessionID string) {
	mode := h.Store.AutoDeleteMode()
	if mode == "none" || a.DeepSeekToken == "" {
		return
	}

	deleteBaseCtx := context.WithoutCancel(ctx)
	deleteCtx, cancel := context.WithTimeout(deleteBaseCtx, 10*time.Second)
	defer cancel()

	switch mode {
	case "single":
		if sessionID == "" {
			config.Logger.Warn("[auto_delete_sessions] skipped single-session delete because session_id is empty", "account", a.AccountID)
			return
		}
		_, err := h.DS.DeleteSessionForToken(deleteCtx, a.DeepSeekToken, sessionID)
		if err != nil {
			config.Logger.Warn("[auto_delete_sessions] failed", "account", a.AccountID, "mode", mode, "session_id", sessionID, "error", err)
			return
		}
		config.Logger.Debug("[auto_delete_sessions] success", "account", a.AccountID, "mode", mode, "session_id", sessionID)
	case "all":
		if err := h.DS.DeleteAllSessionsForToken(deleteCtx, a.DeepSeekToken); err != nil {
			config.Logger.Warn("[auto_delete_sessions] failed", "account", a.AccountID, "mode", mode, "error", err)
			return
		}
		config.Logger.Debug("[auto_delete_sessions] success", "account", a.AccountID, "mode", mode)
	default:
		config.Logger.Warn("[auto_delete_sessions] unknown mode", "account", a.AccountID, "mode", mode)
	}
}

func (h *Handler) handleNonStream(w http.ResponseWriter, resp *http.Response, completionID, model, finalPrompt string, refFileTokens int, thinkingEnabled, searchEnabled bool, toolNames []string, toolsRaw any, historySession *chatHistorySession) {
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if historySession != nil {
			historySession.error(resp.StatusCode, string(body), "error", "", "")
		}
		writeOpenAIError(w, resp.StatusCode, string(body))
		return
	}
	result := sse.CollectStream(resp, thinkingEnabled, true)

	turn := assistantturn.BuildTurnFromCollected(result, assistantturn.BuildOptions{
		Model:         model,
		Prompt:        finalPrompt,
		RefFileTokens: refFileTokens,
		SearchEnabled: searchEnabled,
		ToolNames:     toolNames,
		ToolsRaw:      toolsRaw,
		ToolChoice:    promptcompat.DefaultToolChoicePolicy(),
	})
	outcome := assistantturn.FinalizeTurn(turn, assistantturn.FinalizeOptions{})
	if outcome.ShouldFail {
		status, message, code := outcome.Error.Status, outcome.Error.Message, outcome.Error.Code
		if historySession != nil {
			historySession.error(status, message, code, historyThinkingForArchive(turn.RawThinking, turn.DetectionThinking, turn.Thinking), historyTextForArchive(turn.RawText, turn.Text))
		}
		writeOpenAIErrorWithCode(w, status, message, code)
		return
	}
	respBody := openaifmt.BuildChatCompletionWithToolCalls(completionID, model, finalPrompt, turn.Thinking, turn.Text, turn.ToolCalls, toolsRaw)
	respBody["usage"] = assistantturn.OpenAIChatUsage(turn)
	if historySession != nil {
		historySession.success(http.StatusOK, historyThinkingForArchive(turn.RawThinking, turn.DetectionThinking, turn.Thinking), historyTextForArchive(turn.RawText, turn.Text), outcome.FinishReason, assistantturn.OpenAIChatUsage(turn))
	}
	writeJSON(w, http.StatusOK, respBody)
}

func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request, resp *http.Response, completionID, model, finalPrompt string, refFileTokens int, thinkingEnabled, searchEnabled bool, toolNames []string, toolsRaw any, historySession *chatHistorySession) {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if historySession != nil {
			historySession.error(resp.StatusCode, string(body), "error", "", "")
		}
		writeOpenAIError(w, resp.StatusCode, string(body))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	_, canFlush := w.(http.Flusher)
	if !canFlush {
		config.Logger.Warn("[stream] response writer does not support flush; streaming may be buffered")
	}

	created := time.Now().Unix()
	bufferToolContent := len(toolNames) > 0
	emitEarlyToolDeltas := h.toolcallFeatureMatchEnabled() && h.toolcallEarlyEmitHighConfidence()
	stripReferenceMarkers := stripReferenceMarkersEnabled()
	initialType := "text"
	if thinkingEnabled {
		initialType = "thinking"
	}

	streamRuntime := newChatStreamRuntime(
		w,
		rc,
		canFlush,
		completionID,
		created,
		model,
		finalPrompt,
		thinkingEnabled,
		searchEnabled,
		stripReferenceMarkers,
		toolNames,
		toolsRaw,
		promptcompat.DefaultToolChoicePolicy(),
		bufferToolContent,
		emitEarlyToolDeltas,
	)
	streamRuntime.refFileTokens = refFileTokens

	streamengine.ConsumeSSE(streamengine.ConsumeConfig{
		Context:             r.Context(),
		Body:                resp.Body,
		ThinkingEnabled:     thinkingEnabled,
		InitialType:         initialType,
		KeepAliveInterval:   time.Duration(dsprotocol.KeepAliveTimeout) * time.Second,
		IdleTimeout:         time.Duration(dsprotocol.StreamIdleTimeout) * time.Second,
		MaxKeepAliveNoInput: dsprotocol.MaxKeepaliveCount,
	}, streamengine.ConsumeHooks{
		OnKeepAlive: func() {
			streamRuntime.sendKeepAlive()
		},
		OnParsed: func(parsed sse.LineResult) streamengine.ParsedDecision {
			decision := streamRuntime.onParsed(parsed)
			if historySession != nil {
				historySession.progress(streamRuntime.historyThinking(), streamRuntime.historyText())
			}
			return decision
		},
		OnFinalize: func(reason streamengine.StopReason, _ error) {
			if string(reason) == "content_filter" {
				streamRuntime.finalize("content_filter", false)
			} else {
				streamRuntime.finalize("stop", false)
			}
			if historySession == nil {
				return
			}
			if streamRuntime.finalErrorMessage != "" {
				historySession.error(streamRuntime.finalErrorStatus, streamRuntime.finalErrorMessage, streamRuntime.finalErrorCode, streamRuntime.historyThinking(), streamRuntime.historyText())
				return
			}
			historySession.success(http.StatusOK, streamRuntime.historyThinking(), streamRuntime.historyText(), streamRuntime.finalFinishReason, streamRuntime.finalUsage)
		},
		OnContextDone: func() {
			streamRuntime.markContextCancelled()
			if historySession != nil {
				historySession.stopped(streamRuntime.historyThinking(), streamRuntime.historyText(), string(streamengine.StopReasonContextCancelled))
			}
		},
	})
}

func (h *Handler) handleBrowserProxyChat(w http.ResponseWriter, r *http.Request, a *auth.RequestAuth, stdReq promptcompat.StandardRequest) {
	config.Logger.Info("[browser_proxy] handling chat request", "model", stdReq.ResponseModel, "stream", stdReq.Stream)

	session := h.BrowserSession
	if session == nil {
		writeOpenAIError(w, http.StatusInternalServerError, "browser proxy not initialized")
		return
	}

	config.Logger.Info("[browser_proxy] calling EnsureReady")
	err := session.EnsureReady(r.Context())
	if err != nil {
		config.Logger.Error("[browser_proxy] ensure ready failed", "error", err)
		writeOpenAIError(w, http.StatusServiceUnavailable, fmt.Sprintf("browser not ready: %v", err))
		return
	}
	config.Logger.Info("[browser_proxy] EnsureReady done")

	cfg := browserproxy.NewConfig(h.Store.BrowserProxy())
	chatProxy := browserproxy.NewChatProxy(session, cfg)

	messages := make([]browserproxy.Message, 0, len(stdReq.Messages))
	hasImage := false
	for _, msgAny := range stdReq.Messages {
		msg, ok := msgAny.(map[string]any)
		if !ok {
			continue
		}
		content := ""
		var imageData, imageMimeType string
		switch c := msg["content"].(type) {
		case string:
			content = c
		case []any:
			for _, part := range c {
				if m, ok := part.(map[string]any); ok {
					if text, ok := m["text"].(string); ok {
						content += text
					}
					if m["type"] == "image_url" {
						if imgURL, ok := m["image_url"].(map[string]any); ok {
							if url, ok := imgURL["url"].(string); ok {
								data, mime, err := extractBase64Image(url)
								if err == nil {
									imageData = data
									imageMimeType = mime
									hasImage = true
									config.Logger.Info("[browser_proxy] extracted image",
										"mime_type", mime,
										"data_len", len(data),
									)
								} else {
									config.Logger.Warn("[browser_proxy] failed to extract image", "error", err)
								}
							}
						}
					}
				}
			}
		}
		role, _ := msg["role"].(string)
		if role == "user" {
			content = extractUserText(content)
		}
		config.Logger.Info("[browser_proxy] extracted message",
			"role", role,
			"content_len", len(content),
			"content_preview", content[:min(50, len(content))],
			"content_bytes", fmt.Sprintf("%x", []byte(content[:min(20, len(content))])),
			"has_image", imageData != "",
		)
		messages = append(messages, browserproxy.Message{
			Role:          role,
			Content:       content,
			ImageData:     imageData,
			ImageMimeType: imageMimeType,
		})
	}

	cleanModel := stdReq.ResponseModel
	cleanModel = strings.TrimSuffix(cleanModel, "-browser")
	cleanModel = strings.TrimSuffix(cleanModel, "-direct")

	modelType, _ := config.GetModelType(cleanModel)
	isExpertMode := modelType == "expert"

	req := browserproxy.ChatRequest{
		Messages:   messages,
		Model:      cleanModel,
		Stream:     stdReq.Stream,
		Thinking:   true,
		ExpertMode: isExpertMode,
		Search:     true,
		HasImage:   hasImage,
	}

	config.Logger.Info("[browser_proxy] chat request built",
		"original_model", stdReq.ResponseModel,
		"clean_model", cleanModel,
		"model_type", modelType,
		"expert_mode", isExpertMode,
		"thinking", true,
		"search", true,
		"stream", stdReq.Stream,
	)

	resp, sendErr := chatProxy.SendChat(r.Context(), req)
	if sendErr != nil {
		config.Logger.Error("[browser_proxy] send chat failed", "error", sendErr)
		writeOpenAIError(w, http.StatusInternalServerError, fmt.Sprintf("send message failed: %v", sendErr))
		return
	}

	if resp.Error != nil {
		writeOpenAIError(w, http.StatusInternalServerError, resp.Error.Error())
		return
	}

	if !stdReq.Stream {
		config.Logger.Info("[browser_proxy] waiting for AI response (non-stream)")
		waitErr := chatProxy.WaitForResponseComplete(r.Context())
		if waitErr != nil {
			config.Logger.Warn("[browser_proxy] wait for response warning", "error", waitErr)
		}
		config.Logger.Info("[browser_proxy] response wait complete")
	}

	if stdReq.Stream {
		bridge := browserproxy.NewStreamBridge(w, resp, stdReq.Thinking)
		bridgeErr := bridge.BridgeStream(r.Context(), session)
		if bridgeErr != nil {
			config.Logger.Error("[browser_proxy] stream bridge error", "error", bridgeErr)
		}
	} else {
		result, collectErr := browserproxy.CollectNonStreamResponse(r.Context(), session, resp, stdReq.Thinking)
		if collectErr != nil {
			config.Logger.Error("[browser_proxy] non-stream collect error", "error", collectErr)
			browserproxy.WriteOpenAIError(w, http.StatusInternalServerError, collectErr.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

func extractBase64Image(url string) (data, mimeType string, err error) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return "", "", fmt.Errorf("not a data URL")
	}

	rest := url[len(prefix):]
	commaIdx := strings.Index(rest, ",")
	if commaIdx < 0 {
		return "", "", fmt.Errorf("invalid data URL: no comma")
	}

	mimePart := rest[:commaIdx]
	base64Part := rest[commaIdx+1:]

	if semiIdx := strings.Index(mimePart, ";"); semiIdx >= 0 {
		mimeType = mimePart[:semiIdx]
	} else {
		mimeType = mimePart
	}

	if mimeType == "" {
		mimeType = "image/png"
	}

	return base64Part, mimeType, nil
}

func extractUserText(content string) string {
	if len(content) < 100 {
		return content
	}

	lines := strings.Split(content, "\n")
	nonEmptyLines := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			nonEmptyLines = append(nonEmptyLines, trimmed)
		}
	}

	if len(nonEmptyLines) == 0 {
		return content
	}
	if len(nonEmptyLines) <= 2 {
		return content
	}

	lastLine := nonEmptyLines[len(nonEmptyLines)-1]
	secondLastLine := nonEmptyLines[len(nonEmptyLines)-2]

	isMetadata := func(s string) bool {
		metadataIndicators := []string{
			"Sender (untrusted metadata):",
			"Sender:",
			"[Metadata]",
			"[Sat ",
			"[Sun ",
			"[Mon ",
			"[Tue ",
			"[Wed ",
			"[Thu ",
			"[Fri ",
			"[LobsterAI",
			"```json",
			"```",
			"{",
			"label",
			"Current model:",
			"Current user request",
			"[Current",
			"custom_",
			"/deepseek-",
		}
		for _, indicator := range metadataIndicators {
			if strings.Contains(s, indicator) {
				return true
			}
		}
		return false
	}

	cleanText := func(s string) string {
		s = strings.TrimSpace(s)
		prefixes := []string{
			"Current model: custom_",
			"[Current user request] ",
			"[Current",
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(s, prefix) {
				idx := strings.Index(s, "] ")
				if idx >= 0 {
					s = strings.TrimSpace(s[idx+2:])
				}
			}
		}
		return s
	}

	if isMetadata(secondLastLine) && !isMetadata(lastLine) && len(lastLine) < 200 {
		cleaned := cleanText(lastLine)
		config.Logger.Info("[browser_proxy] extracted last line as user text",
			"original_len", len(content),
			"cleaned_len", len(cleaned),
			"total_lines", len(nonEmptyLines),
		)
		return cleaned
	}

	lastFewLines := strings.Join(nonEmptyLines[max(0, len(nonEmptyLines)-3):], " ")
	if !isMetadata(lastFewLines) && len(lastFewLines) < 300 {
		cleaned := cleanText(lastFewLines)
		config.Logger.Info("[browser_proxy] extracted last few lines as user text",
			"original_len", len(content),
			"cleaned_len", len(cleaned),
		)
		return cleaned
	}

	for i := len(nonEmptyLines) - 1; i >= 0; i-- {
		line := nonEmptyLines[i]
		if !isMetadata(line) && len(line) > 1 && len(line) < 200 {
			cleaned := cleanText(line)
			config.Logger.Info("[browser_proxy] extracted from line index",
				"original_len", len(content),
				"cleaned_len", len(cleaned),
				"line_idx", i,
			)
			return cleaned
		}
	}

	config.Logger.Warn("[browser_proxy] could not extract user text, returning original",
		"content_len", len(content),
		"first_80", content[:min(80, len(content))],
	)

	return content
}

func shouldUseBrowserProxy(model string, browserProxyEnabled bool) bool {
	if strings.HasSuffix(model, "-browser") {
		config.Logger.Info("[mode_detect] browser proxy mode selected",
			"model", model,
			"reason", "model name ends with -browser suffix",
		)
		return true
	}
	if browserProxyEnabled && !strings.Contains(model, "-direct") {
		config.Logger.Info("[mode_detect] browser proxy mode (default safe mode)",
			"model", model,
			"reason", "browser_proxy_enabled=true and no -direct suffix",
		)
		return true
	}
	config.Logger.Info("[mode_detect] direct API mode selected",
		"model", model,
		"reason", "explicit -direct suffix or browser_proxy_disabled",
	)
	return false
}
