package browserproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/chromedp/chromedp"

	"ds2api/internal/config"
)

func randomWait(minMs int, maxMs int) {
	delay := time.Duration(minMs+rand.Intn(maxMs-minMs)) * time.Millisecond
	config.Logger.Info("[chat_proxy] random wait", "delay_ms", delay.Milliseconds())
	time.Sleep(delay)
}

type ChatRequest struct {
	Messages   []Message `json:"messages"`
	Model      string    `json:"model"`
	Stream     bool      `json:"stream"`
	Thinking   bool      `json:"thinking,omitempty"`
	ExpertMode bool      `json:"expert_mode,omitempty"`
	Search     bool      `json:"search,omitempty"`
	HasImage   bool      `json:"has_image,omitempty"`
}

type Message struct {
	Role          string `json:"role"`
	Content       string `json:"content"`
	ImageData     string `json:"image_data,omitempty"`
	ImageMimeType string `json:"image_mime_type,omitempty"`
}

type ChatResponse struct {
	ID      string
	Created int64
	Model   string
	Error   error
	Choices []Choice
}

type Choice struct {
	Index        int
	Message      Message
	FinishReason string
}

type PageMode string

const (
	PageModeQuick   PageMode = "quick"
	PageModeExpert  PageMode = "expert"
	PageModeImage   PageMode = "image"
	PageModeUnknown PageMode = "unknown"
)

type ModeFeatures struct {
	Name          string
	HasFileUpload bool
	HasSearch     bool
	HasThinking   bool
	Description   string
}

var modeFeatureMap = map[PageMode]ModeFeatures{
	PageModeQuick: {
		Name:          "快速模式",
		HasFileUpload: true,
		HasSearch:     true,
		HasThinking:   true,
		Description:   "完整功能：文件上传 + 搜索 + 思考",
	},
	PageModeExpert: {
		Name:          "专家模式",
		HasFileUpload: false,
		HasSearch:     true,
		HasThinking:   true,
		Description:   "限制功能：无文件上传 + 搜索 + 思考",
	},
	PageModeImage: {
		Name:          "识图模式",
		HasFileUpload: true,
		HasSearch:     false,
		HasThinking:   true,
		Description:   "图像功能：文件上传 + 无搜索 + 思考",
	},
	PageModeUnknown: {
		Name:          "未知模式",
		HasFileUpload: false,
		HasSearch:     false,
		HasThinking:   false,
		Description:   "无法确定当前模式特性",
	},
}

type ChatProxy struct {
	session *BrowserSession
	cfg     Config
}

func NewChatProxy(session *BrowserSession, cfg Config) *ChatProxy {
	return &ChatProxy{
		session: session,
		cfg:     cfg,
	}
}

func (cp *ChatProxy) SendChat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	startTime := time.Now()
	config.Logger.Info("[chat_proxy] SendChat started")
	browserCtx := cp.session.BrowserCtx()
	if browserCtx == nil {
		return nil, fmt.Errorf("[chat_proxy] browser not started")
	}

	config.Logger.Info("[chat_proxy] ensuring script injected")
	stepStart := time.Now()
	err := cp.ensureScriptInjected(browserCtx)
	if err != nil {
		return nil, fmt.Errorf("[chat_proxy] script injection failed: %w", err)
	}
	config.Logger.Info("[chat_proxy] script injected", "elapsed_ms", time.Since(stepStart).Milliseconds())

	config.Logger.Info("[chat_proxy] ensuring default modes (deep thinking + smart search)")
	stepStart = time.Now()
	if modeErr := cp.ensureDefaultModes(browserCtx); modeErr != nil {
		config.Logger.Warn("[chat_proxy] ensure default modes warning", "error", modeErr)
	}
	config.Logger.Info("[chat_proxy] default modes ensured", "elapsed_ms", time.Since(stepStart).Milliseconds())

	config.Logger.Info("[chat_proxy] resetting stream state")
	stepStart = time.Now()
	if resetErr := cp.session.ResetStreamState(browserCtx); resetErr != nil {
		config.Logger.Warn("[chat_proxy] reset stream state warning", "error", resetErr)
	}
	config.Logger.Info("[chat_proxy] stream state reset", "elapsed_ms", time.Since(stepStart).Milliseconds())

	config.Logger.Info("[chat_proxy] clearing input")
	stepStart = time.Now()
	err = cp.clearInput(browserCtx)
	if err != nil {
		config.Logger.Warn("[chat_proxy] clear input warning", "error", err)
	}
	config.Logger.Info("[chat_proxy] input cleared", "elapsed_ms", time.Since(stepStart).Milliseconds())

	userMessage := cp.extractUserMessage(req)
	if userMessage == "" && !req.HasImage {
		return nil, fmt.Errorf("[chat_proxy] empty message")
	}

	config.Logger.Info("[chat_proxy] extracted user message",
		"content_len", len(userMessage),
		"content_preview", userMessage[:min(50, len(userMessage))],
		"content_bytes", fmt.Sprintf("%x", []byte(userMessage[:min(20, len(userMessage))])),
		"has_image", req.HasImage,
	)

	currentMode := cp.detectPageMode(browserCtx)
	features := modeFeatureMap[currentMode]
	config.Logger.Info("[chat_proxy] detected page mode",
		"mode", currentMode,
		"mode_name", features.Name,
	)

	targetMode := PageModeQuick
	if req.ExpertMode {
		targetMode = PageModeExpert
	}

	if req.HasImage {
		availableModes := cp.detectAvailableModes(browserCtx)
		hasImageMode := false
		for _, m := range availableModes {
			if m == PageModeImage {
				hasImageMode = true
				break
			}
		}
		if hasImageMode {
			targetMode = PageModeImage
			config.Logger.Info("[chat_proxy] image detected, switching to image mode")
		} else {
			config.Logger.Warn("[chat_proxy] image mode not available for this account, using quick mode")
			targetMode = PageModeQuick
		}
	}

	config.Logger.Info("[chat_proxy] applying mode settings",
		"target_mode", targetMode,
		"thinking", req.Thinking,
		"search", req.Search,
		"has_image", req.HasImage,
	)

	stepStart = time.Now()
	if switchErr := cp.switchPageMode(browserCtx, currentMode, targetMode); switchErr != nil {
		config.Logger.Warn("[chat_proxy] mode switch warning", "error", switchErr)
	}
	config.Logger.Info("[chat_proxy] mode switched", "elapsed_ms", time.Since(stepStart).Milliseconds())

	if req.HasImage {
		stepStart = time.Now()
		imageMsg := cp.extractImageMessage(req)
		if imageMsg != nil {
			config.Logger.Info("[chat_proxy] uploading image",
				"mime_type", imageMsg.ImageMimeType,
				"data_len", len(imageMsg.ImageData),
			)
			if uploadErr := cp.uploadImage(browserCtx, imageMsg.ImageData, imageMsg.ImageMimeType); uploadErr != nil {
				return nil, fmt.Errorf("[chat_proxy] image upload failed: %w", uploadErr)
			}
			config.Logger.Info("[chat_proxy] image uploaded successfully")
		}
		config.Logger.Info("[chat_proxy] image upload done", "elapsed_ms", time.Since(stepStart).Milliseconds())
	}

	config.Logger.Info("[chat_proxy] typing and sending message")
	stepStart = time.Now()
	err = cp.typeAndSend(browserCtx, userMessage)
	if err != nil {
		return nil, fmt.Errorf("[chat_proxy] send message failed: %w", err)
	}
	config.Logger.Info("[chat_proxy] message sent, stream will be collected by caller",
		"elapsed_ms", time.Since(stepStart).Milliseconds(),
		"total_prepare_ms", time.Since(startTime).Milliseconds(),
	)

	resp := &ChatResponse{
		ID:      generateCompletionID(),
		Created: time.Now().Unix(),
		Model:   resolveModel(req.Model),
	}

	return resp, nil
}

func (cp *ChatProxy) detectAvailableModes(ctx context.Context) []PageMode {
	var modes []PageMode
	browserCtx := cp.session.BrowserCtx()
	var modeList []string
	err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`
			(function() {
				var radios = document.querySelectorAll('div[role="radio"]');
				var modes = [];
				for (var i = 0; i < radios.length; i++) {
					var modelType = radios[i].getAttribute('data-model-type') || '';
					if (modelType === 'default') modes.push('quick');
					else if (modelType === 'expert') modes.push('expert');
					else if (modelType === 'image' || modelType === 'vision') modes.push('image');
				}
				return modes;
			})()
		`, &modeList),
	)
	if err != nil {
		config.Logger.Warn("[chat_proxy] detect available modes failed", "error", err)
		return []PageMode{PageModeQuick}
	}
	for _, m := range modeList {
		switch m {
		case "quick":
			modes = append(modes, PageModeQuick)
		case "expert":
			modes = append(modes, PageModeExpert)
		case "image":
			modes = append(modes, PageModeImage)
		}
	}
	if len(modes) == 0 {
		modes = append(modes, PageModeQuick)
	}
	config.Logger.Info("[chat_proxy] available modes", "modes", modes)
	return modes
}

func (cp *ChatProxy) detectPageMode(ctx context.Context) PageMode {
	var modeStr string
	browserCtx := cp.session.BrowserCtx()
	err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`
			(function() {
				var radios = document.querySelectorAll('div[role="radio"]');
				for (var i = 0; i < radios.length; i++) {
					var r = radios[i];
					if (r.getAttribute('aria-checked') !== 'true') continue;
					var modelType = r.getAttribute('data-model-type') || '';
					if (modelType === 'default') return 'quick';
					if (modelType === 'expert') return 'expert';
					if (modelType === 'image' || modelType === 'vision') return 'image';
					var text = (r.textContent || '').trim();
					if (text.indexOf('识图') !== -1 || text.indexOf('图像') !== -1) return 'image';
				}
				return 'unknown';
			})()
		`, &modeStr),
	)

	if err != nil {
		config.Logger.Warn("[chat_proxy] mode detection failed", "error", err)
		return PageModeQuick
	}

	config.Logger.Info("[chat_proxy] detected mode", "mode", modeStr)

	switch modeStr {
	case "expert":
		return PageModeExpert
	case "quick":
		return PageModeQuick
	case "image":
		return PageModeImage
	default:
		return PageModeQuick
	}
}

func (cp *ChatProxy) switchPageMode(ctx context.Context, currentMode, targetMode PageMode) error {
	if currentMode == targetMode {
		config.Logger.Info("[chat_proxy] already in target mode", "mode", targetMode)
		return nil
	}

	config.Logger.Info("[chat_proxy] switching page mode", "from", currentMode, "to", targetMode)

	var selector string
	switch targetMode {
	case PageModeQuick:
		selector = `div[data-model-type="default"][role="radio"]`
	case PageModeExpert:
		selector = `div[data-model-type="expert"][role="radio"]`
	case PageModeImage:
		selector = `div[data-model-type="image"][role="radio"], div[data-model-type="vision"][role="radio"]`
	default:
		return fmt.Errorf("[chat_proxy] unsupported target mode: %s", targetMode)
	}

	browserCtx := cp.session.BrowserCtx()

	var clickResult string
	clickDone := make(chan error, 1)
	go func() {
		clickDone <- chromedp.Run(browserCtx,
			chromedp.Evaluate(fmt.Sprintf(`
				(function() {
					var el = document.querySelector(%q);
					if (el) {
						el.click();
						return 'clicked';
					}
					return 'not_found';
				})()
			`, selector), &clickResult),
		)
	}()

	select {
	case err := <-clickDone:
		if err != nil {
			return fmt.Errorf("[chat_proxy] click mode button failed: %w", err)
		}
	case <-time.After(10 * time.Second):
		return fmt.Errorf("[chat_proxy] click mode button timeout after 10s")
	}

	config.Logger.Info("[chat_proxy] mode button clicked, waiting for page to stabilize", "result", clickResult)

	time.Sleep(200 * time.Millisecond)

	deadline := time.After(20 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			config.Logger.Warn("[chat_proxy] page stabilization timeout after mode switch")
			goto done
		case <-ticker.C:
			var ready bool
			evalDone := make(chan struct{}, 1)
			go func() {
				chromedp.Run(browserCtx,
					chromedp.Evaluate(`!!document.querySelector('textarea')`, &ready),
				)
				evalDone <- struct{}{}
			}()
			select {
			case <-evalDone:
			case <-time.After(3 * time.Second):
				config.Logger.Warn("[chat_proxy] stabilization eval timed out, retrying")
				continue
			}
			if ready {
				config.Logger.Info("[chat_proxy] page stabilized after mode switch")
				goto done
			}
		}
	}

done:
	newMode := cp.detectPageMode(ctx)
	config.Logger.Info("[chat_proxy] mode switch result", "expected", targetMode, "actual", newMode)

	if newMode != targetMode {
		config.Logger.Warn("[chat_proxy] mode switch may have failed", "expected", targetMode, "actual", newMode)
	}

	return nil
}

func (cp *ChatProxy) toggleDeepThinking(ctx context.Context, enable bool) error {
	config.Logger.Info("[chat_proxy] toggling deep thinking", "enable", enable)

	browserCtx := cp.session.BrowserCtx()

	var currentState string
	err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`
			(function() {
				var buttons = document.querySelectorAll('[role="button"][aria-pressed]');
				for (var i = 0; i < buttons.length; i++) {
					if (buttons[i].textContent.indexOf('深度思考') !== -1) {
						return buttons[i].getAttribute('aria-pressed');
					}
				}
				return 'not_found';
			})()
		`, &currentState),
	)

	if err != nil {
		config.Logger.Warn("[chat_proxy] deep thinking state check failed", "error", err)
	}

	config.Logger.Info("[chat_proxy] deep thinking current state", "state", currentState)

	if (currentState == "true") == enable {
		config.Logger.Info("[chat_proxy] deep thinking already in desired state", "enable", enable, "current", currentState)
		return nil
	}

	err = chromedp.Run(browserCtx,
		chromedp.Evaluate(`
			(function() {
				var buttons = document.querySelectorAll('[role="button"][aria-pressed]');
				for (var i = 0; i < buttons.length; i++) {
					if (buttons[i].textContent.indexOf('深度思考') !== -1) {
						buttons[i].click();
						return 'clicked';
					}
				}
				return 'not_found';
			})()
		`, nil),
	)

	if err != nil {
		return fmt.Errorf("[chat_proxy] toggle deep thinking failed: %w", err)
	}

	time.Sleep(100 * time.Millisecond)
	config.Logger.Info("[chat_proxy] deep thinking toggled", "enable", enable)
	return nil
}

func (cp *ChatProxy) toggleSearch(ctx context.Context, enable bool) error {
	config.Logger.Info("[chat_proxy] toggling search", "enable", enable)

	browserCtx := cp.session.BrowserCtx()

	var currentState string
	err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`
			(function() {
				var buttons = document.querySelectorAll('[role="button"][aria-pressed]');
				for (var i = 0; i < buttons.length; i++) {
					if (buttons[i].textContent.indexOf('联网搜索') !== -1 ||
					    buttons[i].textContent.indexOf('智能搜索') !== -1) {
						return buttons[i].getAttribute('aria-pressed');
					}
				}
				return 'not_found';
			})()
		`, &currentState),
	)

	if err != nil {
		config.Logger.Warn("[chat_proxy] search state check failed", "error", err)
	}

	config.Logger.Info("[chat_proxy] search current state", "state", currentState)

	if (currentState == "true") == enable {
		config.Logger.Info("[chat_proxy] search already in desired state", "enable", enable, "current", currentState)
		return nil
	}

	err = chromedp.Run(browserCtx,
		chromedp.Evaluate(`
			(function() {
				var buttons = document.querySelectorAll('[role="button"][aria-pressed]');
				for (var i = 0; i < buttons.length; i++) {
					if (buttons[i].textContent.indexOf('联网搜索') !== -1 ||
					    buttons[i].textContent.indexOf('智能搜索') !== -1) {
						buttons[i].click();
						return 'clicked';
					}
				}
				return 'not_found';
			})()
		`, nil),
	)

	if err != nil {
		return fmt.Errorf("[chat_proxy] toggle search failed: %w", err)
	}

	time.Sleep(100 * time.Millisecond)
	config.Logger.Info("[chat_proxy] search toggled", "enable", enable)
	return nil
}

func (cp *ChatProxy) ensureScriptInjected(ctx context.Context) error {
	browserCtx := cp.session.BrowserCtx()
	if browserCtx == nil {
		return fmt.Errorf("[chat_proxy] browser not available for injection")
	}

	var injected bool
	err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`!!window.__ds2api_injected`, &injected),
	)
	if err != nil {
		return err
	}
	if !injected {
		script, _ := GetInjectionScript()
		chromedp.Run(browserCtx,
			chromedp.Evaluate(script, nil),
		)
	}
	return nil
}

func (cp *ChatProxy) clearInput(ctx context.Context) error {
	browserCtx := cp.session.BrowserCtx()
	return chromedp.Run(browserCtx,
		chromedp.ActionFunc(func(actx context.Context) error {
			return chromedp.Evaluate(`
				(function() {
					var textarea = document.querySelector('textarea');
					if(textarea){
						var nativeInputValueSetter = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set;
						nativeInputValueSetter.call(textarea, '');
						textarea.dispatchEvent(new Event('input', {bubbles: true}));
					}
					var editableDivs = document.querySelectorAll('div[contenteditable="true"]');
					editableDivs.forEach(function(div){ div.innerHTML = ''; });
				})()
			`, nil).Do(actx)
		}),
	)
}

func (cp *ChatProxy) extractUserMessage(req ChatRequest) string {
	if len(req.Messages) == 0 {
		return ""
	}
	lastMsg := req.Messages[len(req.Messages)-1]
	if lastMsg.Role == "user" || lastMsg.Role == "system" {
		return lastMsg.Content
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Content
		}
	}
	return ""
}

func (cp *ChatProxy) extractImageMessage(req ChatRequest) *Message {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].ImageData != "" {
			return &req.Messages[i]
		}
	}
	return nil
}

func (cp *ChatProxy) ensureDefaultModes(ctx context.Context) error {
	config.Logger.Info("[chat_proxy] ensuring default modes enabled")

	if thinkErr := cp.toggleDeepThinking(ctx, true); thinkErr != nil {
		config.Logger.Warn("[chat_proxy] ensure deep thinking warning", "error", thinkErr)
	}

	if searchErr := cp.toggleSearch(ctx, true); searchErr != nil {
		config.Logger.Warn("[chat_proxy] ensure search warning", "error", searchErr)
	}

	config.Logger.Info("[chat_proxy] default modes check complete")
	return nil
}

func (cp *ChatProxy) uploadImage(ctx context.Context, imageData, mimeType string) error {
	config.Logger.Info("[chat_proxy] uploading image via browser",
		"mime_type", mimeType,
		"data_len", len(imageData),
	)

	browserCtx := cp.session.BrowserCtx()

	var uploadResult string
	err := chromedp.Run(browserCtx,
		chromedp.Evaluate(fmt.Sprintf(`
			(async function() {
				try {
					var dataUrl = 'data:%s;base64,%s';

					var resp = await fetch(dataUrl);
					var blob = await resp.blob();
					var file = new File([blob], 'image.' + blob.type.split('/')[1] || 'png', { type: blob.type });

					var dt = new DataTransfer();
					dt.items.add(file);

					var fileInput = document.querySelector('input[type="file"]');
					if (!fileInput) {
						var inputs = document.querySelectorAll('input');
						for (var i = 0; i < inputs.length; i++) {
							if (inputs[i].type === 'file') {
								fileInput = inputs[i];
								break;
							}
						}
					}

					if (!fileInput) {
						return 'error:no_file_input_found';
					}

					fileInput.files = dt.files;
					fileInput.dispatchEvent(new Event('change', { bubbles: true }));
					fileInput.dispatchEvent(new Event('input', { bubbles: true }));

					return 'uploaded:' + file.name;
				} catch(e) {
					return 'error:' + e.message;
				}
			})()
		`, mimeType, imageData), &uploadResult),
	)

	if err != nil {
		return fmt.Errorf("[chat_proxy] image upload JS error: %w", err)
	}

	config.Logger.Info("[chat_proxy] image upload result", "result", uploadResult)

	if strings.HasPrefix(uploadResult, "error:") {
		return fmt.Errorf("[chat_proxy] image upload failed: %s", uploadResult)
	}

	time.Sleep(500 * time.Millisecond)

	return nil
}

func (cp *ChatProxy) typeAndSend(ctx context.Context, message string) error {
	browserCtx := cp.session.BrowserCtx()
	err := chromedp.Run(browserCtx,
		chromedp.ActionFunc(func(actx context.Context) error {
			var inputSelector string
			var inputExists bool

			textareaSelectors := []string{
				`textarea[placeholder*="DeepSeek"]`,
				`textarea[placeholder*="发送消息"]`,
				`textarea[placeholder*="message"]`,
				`textarea`,
				`div[contenteditable="true"][class*="input"]`,
				`div[contenteditable="true"][class*="editor"]`,
				`div[data-testid="chat-input"]`,
			}

			for _, sel := range textareaSelectors {
				chromedp.Evaluate(`!!document.querySelector('`+sel+`')`, &inputExists).Do(actx)
				if inputExists {
					inputSelector = sel
					break
				}
			}

			if !inputExists || inputSelector == "" {
				return fmt.Errorf("input element not found")
			}

			config.Logger.Info("[chat_proxy] using input selector", "selector", inputSelector)

			var elInfo string
			chromedp.Evaluate(`
				(function() {
					var el = document.querySelector('`+inputSelector+`');
					if (!el) return 'not_found';
					return 'tag=' + el.tagName + ' type=' + (el.type || '') + ' contentEditable=' + el.contentEditable + ' class=' + (el.className || '').substring(0, 80);
				})()
			`, &elInfo).Do(actx)
			config.Logger.Info("[chat_proxy] element info", "info", elInfo)

			clickErr := chromedp.Click(inputSelector, chromedp.ByQuery).Do(actx)
			if clickErr != nil {
				return fmt.Errorf("click input failed: %w", clickErr)
			}

			config.Logger.Info("[chat_proxy] typing message via JS", "message_len", len(message), "message_preview", message[:min(50, len(message))])

			jsonBytes, _ := json.Marshal(message)
			jsonMessage := string(jsonBytes)

			var setResult string
			chromedp.Evaluate(`
				(function() {
					var el = document.querySelector('`+inputSelector+`');
					if (!el) return 'error:element_not_found';

					el.click();
					el.focus();

					var msg = `+jsonMessage+`;

					var nativeInputValueSetter = Object.getOwnPropertyDescriptor(
						window.HTMLTextAreaElement.prototype, 'value'
					).set || Object.getOwnPropertyDescriptor(
						window.HTMLInputElement.prototype, 'value'
					).set;

					nativeInputValueSetter.call(el, '');

					el.dispatchEvent(new Event('input', { bubbles: true, composed: true }));

					nativeInputValueSetter.call(el, msg);

					el.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
					el.dispatchEvent(new Event('change', { bubbles: true, composed: true }));

					var reactKey = Object.keys(el).find(function(k) { return k.startsWith('__reactFiber$'); });
					if (reactKey) {
						var fiber = el[reactKey];
						while (fiber && !fiber.stateNode) {
							fiber = fiber.return;
						}
						if (fiber && fiber.memoizedProps && fiber.memoizedProps.hasOwnProperty('value')) {
							fiber.memoizedProps.value = msg;
						}
					}

					var result = 'tag=' + el.tagName + ' val_len=' + (el.value || el.textContent || '').length;
					result += ' val_preview=' + (el.value || el.textContent || '').substring(0, 50);
					return result;
				})()
			`, &setResult).Do(actx)

			config.Logger.Info("[chat_proxy] JS set result", "result", setResult)

			randomWait(200, 500)

			return clickSendButton(actx, inputSelector)
		}),
	)

	return err
}

func (cp *ChatProxy) waitForInputReady(ctx context.Context) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	attempt := 0
	for {
		select {
		case <-timeoutCtx.Done():
			var pageURL string
			var pageText string
			chromedp.Evaluate(`window.location.href`, &pageURL).Do(ctx)
			chromedp.Evaluate(`document.body ? document.body.innerText.substring(0, 300) : 'no body'`, &pageText).Do(ctx)
			config.Logger.Error("[chat_proxy] input timeout",
				"attempts", attempt,
				"url", pageURL,
				"page_preview", pageText[:min(200, len(pageText))],
			)
			return fmt.Errorf("[chat_proxy] timeout waiting for input element")
		case <-ticker.C:
			attempt++
			var ready bool
			chromedp.Evaluate(`
				(function() {
					var selectors = [
						'textarea',
						'div[contenteditable="true"]',
						'input[type="text"]'
					];
					for(var i=0; i<selectors.length; i++){
						if(document.querySelector(selectors[i])) return true;
					}
					return false;
				})()
			`, &ready).Do(timeoutCtx)

			if ready {
				config.Logger.Info("[chat_proxy] input ready", "attempts", attempt)
				time.Sleep(200 * time.Millisecond)
				return nil
			}

			if attempt%10 == 0 {
				var pageURL string
				chromedp.Evaluate(`window.location.href`, &pageURL).Do(timeoutCtx)
				config.Logger.Info("[chat_proxy] waiting for input", "attempt", attempt, "url", pageURL)
			}
		}
	}
}

func clickSendButton(ctx context.Context, inputSelector string) error {
	config.Logger.Info("[chat_proxy] looking for send button...", "input_selector", inputSelector)

	var sendResult string

	chromedp.Evaluate(`
		(function() {
			try {
				var textarea = document.querySelector('`+inputSelector+`');
				if(!textarea) return 'error:textarea_not_found';
				
				var textareaRect = textarea.getBoundingClientRect();
				
				var allButtons = document.querySelectorAll('div[role="button"].ds-icon-button, .ds-icon-button__hover-bg');
				
				var closestButton = null;
				var minDistance = Infinity;
				
				for(var i=0; i<allButtons.length; i++){
					var btn = allButtons[i];
					var btnRect = btn.getBoundingClientRect();
					
					var isDisabled = btn.getAttribute('aria-disabled') === 'true' || btn.disabled;
					
					if(isDisabled) continue;
					
					var hasSVG = !!btn.querySelector('svg');
					if(!hasSVG) continue;
					
					var isBelowTextarea = btnRect.top > textareaRect.top - 50;
					var isNearby = Math.abs(btnRect.left - textareaRect.right) < 200 || 
									(btnRect.left > textareaRect.left && btnRect.top < textareaRect.bottom + 100);
					
					if(isBelowTextarea || isNearby){
						var distance = Math.abs(btnRect.top - textareaRect.top) + 
									   Math.abs(btnRect.left - textareaRect.right);
						
						if(distance < minDistance){
							minDistance = distance;
							closestButton = btn;
						}
					}
				}
				
				if(closestButton){
					closestButton.click();
					return 'clicked_send_button:' + closestButton.className.substring(0,60);
				}
				
				var parentContainer = textarea.closest('[class*="input"], [class*="chat"], [class*="composer"]');
				if(parentContainer){
					var containerBtns = parentContainer.querySelectorAll('div[role="button"], button, .ds-icon-button');
					for(var j=0; j<containerBtns.length; j++){
						var cBtn = containerBtns[j];
						if(cBtn.getAttribute('aria-disabled') !== 'true' && !cBtn.disabled){
							if(cBtn.querySelector('svg') || cBtn.classList.contains('icon-button')){
								cBtn.click();
								return 'clicked_container_btn:' + cBtn.className.substring(0,60);
							}
						}
					}
				}
				
				return 'send_button_not_found';
			} catch(e) {
				return 'error:' + e.message;
			}
		})()
	`, &sendResult).Do(ctx)

	config.Logger.Info("[chat_proxy] send button result", "result", sendResult)

	if strings.HasPrefix(sendResult, "clicked") {
		config.Logger.Info("[chat_proxy] ✅ send button clicked, checking reply immediately")
		return nil
	}

	config.Logger.Warn("[button] failed, trying keyboard Enter...")

	var keyboardResult string
	chromedp.Evaluate(`
		(function() {
			try {
				var textarea = document.querySelector('`+inputSelector+`');
				if(!textarea) return 'error:textarea_not_found';
				
				textarea.focus();
				textarea.select();
				
				var enterEvent = new KeyboardEvent('keydown', {
					key: 'Enter',
					code: 'Enter',
					keyCode: 13,
					which: 13,
					bubbles: true,
					cancelable: true,
					composed: true
				});
				
				var prevented = !textarea.dispatchEvent(enterEvent);
				
				setTimeout(function(){
					var enterUpEvent = new KeyboardEvent('keyup', {
						key: 'Enter',
						code: 'Enter',
						bubbles: true
					});
					textarea.dispatchEvent(enterUpEvent);
				}, 10);
				
				return prevented ? 'enter_prevented' : 'enter_dispatched';
			} catch(e) {
				return 'error:' + e.message;
			}
		})()
	`, &keyboardResult).Do(ctx)

	config.Logger.Info("[keyboard] result", "result", keyboardResult)

	config.Logger.Info("[chat_proxy] ✅ Enter key dispatched, checking reply immediately")

	return nil
}

func (cp *ChatProxy) WaitForResponseComplete(ctx context.Context) error {
	return cp.waitForResponseComplete(ctx)
}

func (cp *ChatProxy) waitForResponseComplete(ctx context.Context) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	startTime := time.Now()
	pollCount := 0

	var initialPageText string
	cp.session.ExecuteJS(ctx, `document.body ? document.body.innerText : ''`, &initialPageText)
	config.Logger.Info("[response] monitoring start", "initial_len", len(initialPageText))

	responseDetected := false
	lastPageLen := len(initialPageText)
	stableCount := 0
	const stableThreshold = 3

	for {
		select {
		case <-timeoutCtx.Done():
			config.Logger.Error("[response] timeout",
				"elapsed", time.Since(startTime).Seconds(),
				"poll_count", pollCount,
				"response_detected", responseDetected,
				"initial_len", len(initialPageText),
			)
			return fmt.Errorf("[response] timeout after %v", time.Since(startTime))
		case <-ticker.C:
			pollCount++

			var pageText string
			cp.session.ExecuteJS(timeoutCtx, `
				(function() {
					return document.body ? document.body.innerText : '';
				})()
			`, &pageText)

			currentLen := len(pageText)
			delta := currentLen - len(initialPageText)

			if delta > 10 && !responseDetected {
				responseDetected = true
				config.Logger.Info("[response] 🔍 AI starting to respond",
					"elapsed_ms", time.Since(startTime).Milliseconds(),
					"delta", delta,
				)
			}

			if responseDetected {
				if currentLen == lastPageLen {
					stableCount++
					if stableCount >= stableThreshold {
						config.Logger.Info("[response] ✅ response stabilized",
							"elapsed_ms", time.Since(startTime).Milliseconds(),
							"stable_count", stableCount,
							"final_len", currentLen,
							"total_delta", delta,
						)

						cleanContent := cp.extractAIResponseFromDOM(timeoutCtx)

						if cleanContent == "" {
							config.Logger.Warn("[response] DOM extraction empty, falling back to text diff")
							newText := pageText[len(initialPageText):]
							cleanContent = cp.extractCleanResponseFallback(newText)
						}

						config.Logger.Info("[response] 📝 extracted response",
							"content_length", len(cleanContent),
							"content", cleanContent[:min(300, len(cleanContent))],
						)

						cp.session.StoreResponse(cleanContent)
						return nil
					}
				} else {
					stableCount = 0
					lastPageLen = currentLen
				}
			}

			if pollCount%15 == 0 {
				config.Logger.Info("[response] polling",
					"sec", time.Since(startTime).Seconds(),
					"poll", pollCount,
					"detected", responseDetected,
					"delta", delta,
					"stable", stableCount,
				)
			}
		}
	}
}

func (cp *ChatProxy) extractAIResponseFromDOM(ctx context.Context) string {
	var result string
	err := cp.session.ExecuteJS(ctx, `
		(function() {
			var markdownBlocks = document.querySelectorAll('.ds-markdown');
			if (markdownBlocks.length > 0) {
				var lastBlock = markdownBlocks[markdownBlocks.length - 1];
				var text = (lastBlock.innerText || lastBlock.textContent || '').trim();
				if (text.length > 0) return text;
			}

			var aiMessages = document.querySelectorAll('[class*="assistant"], [class*="bot"], [class*="ai"]');
			for (var i = aiMessages.length - 1; i >= 0; i--) {
				var text = (aiMessages[i].innerText || aiMessages[i].textContent || '').trim();
				if (text.length > 0) return text;
			}

			var allMessages = document.querySelectorAll('[class*="message"], [class*="bubble"], [class*="turn"]');
			if (allMessages.length > 0) {
				var lastMsg = allMessages[allMessages.length - 1];
				var text = (lastMsg.innerText || lastMsg.textContent || '').trim();
				if (text.length > 0) return text;
			}

			return '';
		})()
	`, &result)
	if err != nil {
		config.Logger.Warn("[response] DOM extraction JS error", "error", err)
		return ""
	}

	config.Logger.Info("[response] DOM extraction result",
		"length", len(result),
		"preview", result[:min(200, len(result))],
	)
	return result
}

func (cp *ChatProxy) extractCleanResponseFallback(newText string) string {
	lines := strings.Split(newText, "\n")
	var cleanLines []string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) < 1 {
			continue
		}

		lower := strings.ToLower(line)
		if strings.Contains(lower, "内容由 ai 生成") || strings.Contains(lower, "请仔细甄别") {
			continue
		}

		cleanLines = append(cleanLines, line)
	}

	return strings.Join(cleanLines, "\n")
}

func generateCompletionID() string {
	return fmt.Sprintf("chatcmpl-browser-%d", time.Now().UnixNano())
}

func resolveModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "deepseek-chat"
	}
	return model
}

func FormatMessagesForDisplay(messages []Message) string {
	if len(messages) == 0 {
		return ""
	}
	var builder strings.Builder
	for _, msg := range messages {
		builder.WriteString(fmt.Sprintf("[%s]: %s\n", msg.Role, msg.Content[:min(50, len(msg.Content))]))
	}
	return builder.String()
}

func MarshalMessages(messages []Message) ([]byte, error) {
	return json.Marshal(messages)
}
