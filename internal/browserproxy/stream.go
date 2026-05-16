package browserproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ds2api/internal/config"
	"ds2api/internal/sse"
)

type StreamBridge struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	flushed bool
	mu      sync.Mutex

	completionID string
	created      int64
	model        string
	thinking     bool

	totalPromptTokens     int
	totalCompletionTokens int
}

type OpenAIStreamChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []OpenAIStreamChoice `json:"choices"`
}

type OpenAIStreamChoice struct {
	Index        int                    `json:"index"`
	Delta        map[string]interface{} `json:"delta"`
	FinishReason *string                `json:"finish_reason,omitempty"`
}

type OpenAINonStreamResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

type OpenAIChoice struct {
	Index        int       `json:"index"`
	Message      AIMessage `json:"message"`
	FinishReason string    `json:"finish_reason"`
}

type AIMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func NewStreamBridge(w http.ResponseWriter, resp *ChatResponse, thinking bool) *StreamBridge {
	return &StreamBridge{
		w:            w,
		rc:           http.NewResponseController(w),
		completionID: resp.ID,
		created:      resp.Created,
		model:        resp.Model,
		thinking:     thinking,
	}
}

func (b *StreamBridge) BridgeStream(ctx context.Context, session *BrowserSession) error {
	b.setSSEHeaders()

	config.Logger.Info("[stream_bridge] starting CDP-based streaming")

	timeoutCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	startTime := time.Now()
	var allChunks []string
	pollCount := 0

	for {
		select {
		case <-timeoutCtx.Done():
			config.Logger.Error("[stream_bridge] timeout",
				"elapsed", time.Since(startTime).Seconds(),
				"poll_count", pollCount,
				"total_chunks", len(allChunks),
			)
			b.emitError("stream timeout")
			return fmt.Errorf("[stream_bridge] timeout after %v", time.Since(startTime))
		case <-ticker.C:
			pollCount++

			newChunks, cdpDone := session.PollCDPChunks()
			if len(newChunks) > 0 {
				for _, raw := range newChunks {
					raw = strings.TrimSpace(raw)
					if raw == "" || raw == "[DONE]" {
						continue
					}

					sseLine := "data: " + raw
					chunk, _, valid := sse.ParseDeepSeekSSELine([]byte(sseLine))
					if !valid {
						continue
					}

					parts, finished, _ := sse.ParseSSEChunkForContent(chunk, b.thinking, "")
					for _, part := range parts {
						if part.Type == "text" && part.Text != "" {
							b.emitTextChunk(part.Text)
						}
					}

					if finished {
						config.Logger.Info("[stream_bridge] stream finished via SSE",
							"elapsed_ms", time.Since(startTime).Milliseconds(),
							"total_chunks", len(allChunks)+len(newChunks),
						)
						b.emitDone()
						return nil
					}
				}
				allChunks = append(allChunks, newChunks...)
			}

			if cdpDone && len(allChunks) > 0 {
				config.Logger.Info("[stream_bridge] CDP done with chunks",
					"elapsed_ms", time.Since(startTime).Milliseconds(),
					"total_chunks", len(allChunks),
				)
				b.emitDone()
				return nil
			}

			if pollCount%25 == 0 {
				config.Logger.Info("[stream_bridge] polling",
					"sec", time.Since(startTime).Seconds(),
					"poll", pollCount,
					"chunks", len(allChunks),
					"cdp_done", cdpDone,
				)
			}
		}
	}
}

func (b *StreamBridge) extractCleanStreamText(text string) string {
	lines := strings.Split(text, "\n")
	var cleanLines []string

	excludeKeywords := []string{
		"快速模式", "专家模式", "深度思考", "智能搜索",
		"内容由 AI", "请仔细甄别", "开启新对话", "新对话",
		"已思考",
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) < 2 {
			continue
		}

		excluded := false
		for _, keyword := range excludeKeywords {
			if strings.Contains(line, keyword) {
				excluded = true
				break
			}
		}

		if !excluded {
			cleanLines = append(cleanLines, line)
		}
	}

	return strings.Join(cleanLines, "\n")
}

func (b *StreamBridge) emitTextChunk(text string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	chunk := b.buildTextChunk(text)
	b.writeSSE(chunk)
}

func (b *StreamBridge) waitForStreamStart(ctx context.Context, session *BrowserSession) error {
	const streamStartTimeout = 60 * time.Second
	timeoutCtx, cancel := context.WithTimeout(ctx, streamStartTimeout)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	startTime := time.Now()
	attempt := 0

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("[stream_bridge] timeout waiting for stream to start after %v", time.Since(startTime))
		case <-ticker.C:
			attempt++

			var status string
			session.ExecuteJS(timeoutCtx, `window.__ds2api_status`, &status)

			if attempt <= 3 || attempt%25 == 0 {
				var debugLen int
				session.ExecuteJS(timeoutCtx, `(window.__ds2api_debug || []).length`, &debugLen)
				var urlsLen int
				session.ExecuteJS(timeoutCtx, `(window.__ds2api_all_urls || []).length`, &urlsLen)
				var currentURL string
				session.ExecuteJS(timeoutCtx, `window.location.href`, &currentURL)
				var injected bool
				session.ExecuteJS(timeoutCtx, `!!window.__ds2api_injected`, &injected)
				var fetchHooked bool
				session.ExecuteJS(timeoutCtx, `!!window.__ds2api_fetch_hooked`, &fetchHooked)
				var allDebugLogs string
				session.ExecuteJS(timeoutCtx, `(window.__ds2api_debug || []).join('\n')`, &allDebugLogs)
				var allUrlsStr string
				session.ExecuteJS(timeoutCtx, `(window.__ds2api_all_urls || []).join('\n')`, &allUrlsStr)
				config.Logger.Info("[stream_bridge] poll",
					"attempt", attempt,
					"status", status,
					"debug_len", debugLen,
					"urls_len", urlsLen,
					"current_url", currentURL,
					"injected", injected,
					"fetch_hooked", fetchHooked,
				)
				if allDebugLogs != "" {
					config.Logger.Info("[stream_bridge] ALL_DEBUG", "logs", allDebugLogs)
				}
				if allUrlsStr != "" {
					config.Logger.Info("[stream_bridge] ALL_URLS", "urls", allUrlsStr)
				}
			}

			if status == "streaming" {
				config.Logger.Info("[stream_bridge] stream started",
					"wait_ms", time.Since(startTime).Milliseconds(),
				)
				return nil
			}

			if status == "error" {
				var errMsg string
				session.ExecuteJS(timeoutCtx, `window.__ds2api_error`, &errMsg)
				return fmt.Errorf("[stream_bridge] stream error: %s", errMsg)
			}

			if status == "done" {
				chunks, pollErr := session.PollChunks(timeoutCtx)
				if pollErr == nil && len(chunks) > 0 {
					config.Logger.Info("[stream_bridge] stream already done with data",
						"chunks", len(chunks),
					)
					return nil
				}
				return fmt.Errorf("[stream_bridge] stream ended before any data")
			}

			if attempt%25 == 0 {
				config.Logger.Info("[stream_bridge] waiting for stream start",
					"elapsed_ms", time.Since(startTime).Milliseconds(),
					"status", status,
				)
			}
		}
	}
}

func (b *StreamBridge) pollAndEmitWithCDP(ctx context.Context, session *BrowserSession) (bool, error) {
	cdpChunks, cdpDone := session.PollCDPChunks()
	if len(cdpChunks) > 0 {
		config.Logger.Info("[stream_bridge] CDP chunks received", "count", len(cdpChunks))
		for _, raw := range cdpChunks {
			isDone, emitErr := b.processRawChunk(raw)
			if emitErr != nil {
				config.Logger.Warn("[stream_bridge] CDP chunk process error", "error", emitErr)
				continue
			}
			if isDone {
				return true, nil
			}
		}
	}
	if cdpDone {
		b.emitDone()
		return true, nil
	}

	chunks, err := session.PollChunks(ctx)
	if err != nil {
		return false, fmt.Errorf("[stream_bridge] poll failed: %w", err)
	}

	for _, raw := range chunks {
		isDone, emitErr := b.processRawChunk(raw)
		if emitErr != nil {
			config.Logger.Warn("[stream_bridge] chunk process error", "error", emitErr)
			continue
		}
		if isDone {
			return true, nil
		}
	}

	var status string
	session.ExecuteJS(ctx, `window.__ds2api_status`, &status)
	if status == "done" || status == "error" {
		if status == "error" {
			var errMsg string
			session.ExecuteJS(ctx, `window.__ds2api_error`, &errMsg)
			b.emitError(fmt.Sprintf("stream error: %s", errMsg))
			return true, fmt.Errorf("%s", errMsg)
		}
		b.emitDone()
		return true, nil
	}

	var streamDone bool
	session.ExecuteJS(ctx, `window.__ds2api_done`, &streamDone)
	if streamDone {
		b.emitDone()
		return true, nil
	}

	return false, nil
}

func (b *StreamBridge) pollAndEmit(ctx context.Context, session *BrowserSession) (bool, error) {
	chunks, err := session.PollChunks(ctx)
	if err != nil {
		return false, fmt.Errorf("[stream_bridge] poll failed: %w", err)
	}

	for _, raw := range chunks {
		isDone, emitErr := b.processRawChunk(raw)
		if emitErr != nil {
			config.Logger.Warn("[stream_bridge] chunk process error", "error", emitErr)
			continue
		}
		if isDone {
			return true, nil
		}
	}

	var status string
	session.ExecuteJS(ctx, `window.__ds2api_status`, &status)
	if status == "done" || status == "error" {
		if status == "error" {
			var errMsg string
			session.ExecuteJS(ctx, `window.__ds2api_error`, &errMsg)
			b.emitError(fmt.Sprintf("stream error: %s", errMsg))
			return true, fmt.Errorf("%s", errMsg)
		}
		b.emitDone()
		return true, nil
	}

	var streamDone bool
	session.ExecuteJS(ctx, `window.__ds2api_done`, &streamDone)
	if streamDone {
		b.emitDone()
		return true, nil
	}

	return false, nil
}

func (b *StreamBridge) processRawChunk(rawData string) (bool, error) {
	rawData = strings.TrimSpace(rawData)
	if rawData == "" {
		return false, nil
	}
	if rawData == "[DONE]" {
		b.emitDone()
		return true, nil
	}

	var chunk map[string]any
	if strings.HasPrefix(rawData, "data:") {
		parsed, _, valid := sse.ParseDeepSeekSSELine([]byte(rawData))
		if !valid {
			return false, nil
		}
		chunk = parsed
	} else {
		chunk = map[string]any{}
		if err := json.Unmarshal([]byte(rawData), &chunk); err != nil {
			previewLen := len(rawData)
			if previewLen > 80 {
				previewLen = 80
			}
			config.Logger.Debug("[stream_bridge] chunk parse failed", "raw_preview", rawData[:previewLen])
			return false, nil
		}
	}

	parts, finished, _ := sse.ParseSSEChunkForContent(chunk, b.thinking, "")

	if len(parts) > 0 {
		b.emitContentParts(parts)
	}

	if finished {
		b.emitDone()
		return true, nil
	}

	return false, nil
}

func (b *StreamBridge) setSSEHeaders() {
	header := b.w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	b.w.WriteHeader(http.StatusOK)
	b.flushed = true
}

func (b *StreamBridge) emitContentParts(parts []sse.ContentPart) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, part := range parts {
		switch part.Type {
		case "thinking":
			if b.thinking {
				chunk := b.buildThinkingChunk(part.Text)
				b.writeSSE(chunk)
			}
		case "text":
			chunk := b.buildTextChunk(part.Text)
			b.writeSSE(chunk)

			b.totalCompletionTokens += len([]rune(part.Text))
		}
	}
}

func (b *StreamBridge) buildThinkingChunk(text string) OpenAIStreamChunk {
	return OpenAIStreamChunk{
		ID:      b.completionID,
		Object:  "chat.completion.chunk",
		Created: b.created,
		Model:   b.model,
		Choices: []OpenAIStreamChoice{
			{
				Index: 0,
				Delta: map[string]interface{}{
					"reasoning_content": text,
				},
			},
		},
	}
}

func (b *StreamBridge) buildTextChunk(text string) OpenAIStreamChunk {
	return OpenAIStreamChunk{
		ID:      b.completionID,
		Object:  "chat.completion.chunk",
		Created: b.created,
		Model:   b.model,
		Choices: []OpenAIStreamChoice{
			{
				Index: 0,
				Delta: map[string]interface{}{
					"content": text,
				},
			},
		},
	}
}

func (b *StreamBridge) emitDone() {
	b.mu.Lock()
	defer b.mu.Unlock()

	doneStr := "stop"
	chunk := OpenAIStreamChunk{
		ID:      b.completionID,
		Object:  "chat.completion.chunk",
		Created: b.created,
		Model:   b.model,
		Choices: []OpenAIStreamChoice{
			{
				Index:        0,
				Delta:        map[string]interface{}{},
				FinishReason: &doneStr,
			},
		},
	}
	b.writeSSE(chunk)
	fmt.Fprintf(b.w, "data: [DONE]\n\n")
	b.flush()
}

func (b *StreamBridge) emitError(message string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	errMsg := fmt.Sprintf(`{"error":{"message":"%s","type":"browser_proxy_error"}}`, message)
	fmt.Fprintf(b.w, "data: %s\n\n", errMsg)
	b.flush()
}

func (b *StreamBridge) writeSSE(chunk interface{}) {
	data, err := json.Marshal(chunk)
	if err != nil {
		config.Logger.Warn("[stream_bridge] marshal error", "error", err)
		return
	}
	fmt.Fprintf(b.w, "data: %s\n\n", data)
	b.flush()
}

func (b *StreamBridge) flush() {
	if !b.flushed || b.rc == nil {
		return
	}
	err := b.rc.Flush()
	if err != nil {
		config.Logger.Warn("[stream_bridge] flush error", "error", err)
	}
}

func CollectNonStreamResponse(ctx context.Context, session *BrowserSession, resp *ChatResponse, thinking bool) (*OpenAINonStreamResponse, error) {
	domResp, hasDomResp := session.GetDOMResponse()
	if hasDomResp && domResp.Content != "" {
		config.Logger.Info("[non_stream] using DOM response directly",
			"content_length", len(domResp.Content),
		)
		session.ClearDOMResponse()
		return buildNonStreamResponse(resp, domResp.Content, ""), nil
	}

	var allContent strings.Builder
	var allThinking strings.Builder
	timeoutCtx, cancel := context.WithTimeout(ctx, session.cfg.Timeout())
	defer cancel()

	ticker := time.NewTicker(session.cfg.PollInterval())
	defer ticker.Stop()

	startTime := time.Now()

	for {
		select {
		case <-timeoutCtx.Done():
			config.Logger.Error("[non_stream] timeout",
				"elapsed", time.Since(startTime).Seconds(),
				"content_len", allContent.Len(),
			)
			return nil, fmt.Errorf("[non_stream] timeout collecting response after %v", time.Since(startTime))
		case <-ticker.C:
			chunks, err := session.PollChunks(timeoutCtx)
			if err != nil {
				continue
			}

			for _, raw := range chunks {
				rawData := strings.TrimSpace(raw)
				if rawData == "" {
					continue
				}
				if rawData == "[DONE]" {
					config.Logger.Info("[non_stream] response complete",
						"elapsed_ms", time.Since(startTime).Milliseconds(),
						"content_len", allContent.Len(),
					)
					return buildNonStreamResponse(resp, allContent.String(), allThinking.String()), nil
				}

				chunk, _, valid := sse.ParseDeepSeekSSELine([]byte(rawData))
				if !valid {
					continue
				}

				parts, finished, _ := sse.ParseSSEChunkForContent(chunk, thinking, "")
				for _, part := range parts {
					switch part.Type {
					case "thinking":
						allThinking.WriteString(part.Text)
					case "text":
						allContent.WriteString(part.Text)
					}
				}

				if finished {
					config.Logger.Info("[non_stream] response finished",
						"elapsed_ms", time.Since(startTime).Milliseconds(),
						"content_len", allContent.Len(),
					)
					return buildNonStreamResponse(resp, allContent.String(), allThinking.String()), nil
				}
			}

			var status string
			session.ExecuteJS(timeoutCtx, `window.__ds2api_status`, &status)
			if status == "done" || status == "error" {
				if status == "error" {
					var errMsg string
					session.ExecuteJS(timeoutCtx, `window.__ds2api_error`, &errMsg)
					return nil, fmt.Errorf("stream error: %s", errMsg)
				}
				config.Logger.Info("[non_stream] stream done",
					"elapsed_ms", time.Since(startTime).Milliseconds(),
					"content_len", allContent.Len(),
				)
				return buildNonStreamResponse(resp, allContent.String(), allThinking.String()), nil
			}

			var streamDone bool
			session.ExecuteJS(timeoutCtx, `window.__ds2api_done`, &streamDone)
			if streamDone {
				config.Logger.Info("[non_stream] stream done flag",
					"elapsed_ms", time.Since(startTime).Milliseconds(),
					"content_len", allContent.Len(),
				)
				return buildNonStreamResponse(resp, allContent.String(), allThinking.String()), nil
			}
		}
	}
}

func buildNonStreamResponse(resp *ChatResponse, content, thinking string) *OpenAINonStreamResponse {
	message := AIMessage{
		Role:    "assistant",
		Content: content,
	}
	if thinking != "" {
		message.ReasoningContent = thinking
	}

	promptTokens := estimateTokenCount(content + thinking)
	completionTokens := promptTokens

	result := &OpenAINonStreamResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: resp.Created,
		Model:   resp.Model,
		Choices: []OpenAIChoice{
			{
				Index:        0,
				Message:      message,
				FinishReason: "stop",
			},
		},
		Usage: OpenAIUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}

	return result
}

func estimateTokenCount(text string) int {
	runes := []rune(text)
	count := len(runes)/3 + 1
	if count < 1 {
		count = 1
	}
	return count
}

func WriteOpenAIError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	errorResp := struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    int    `json:"code"`
		} `json:"error"`
	}{}
	errorResp.Error.Message = message
	errorResp.Error.Type = "browser_proxy_error"
	errorResp.Error.Code = statusCode

	json.NewEncoder(w).Encode(errorResp)
}
