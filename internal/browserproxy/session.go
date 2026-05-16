package browserproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"ds2api/internal/config"
)

type DOMResponse struct {
	Content   string
	Timestamp time.Time
}

type BrowserSession struct {
	account config.Account
	cfg     Config

	mu            sync.Mutex
	allocCtx      context.Context
	allocCancel   context.CancelFunc
	browserCtx    context.Context
	browserCancel context.CancelFunc

	ready       bool
	loggedInAt  time.Time
	lastUsedAt  time.Time
	domResponse *DOMResponse
	netLogs     []string

	cdpChunks   []string
	cdpDone     bool
	cdpStreamID string

	netListenerReady bool
}

func NewBrowserSession(cfg Config, account config.Account) *BrowserSession {
	return &BrowserSession{
		account: account,
		cfg:     cfg,
	}
}

func (s *BrowserSession) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.browserCtx != nil {
		return nil
	}

	return s.startLocked(ctx)
}

func (s *BrowserSession) startLocked(ctx context.Context) error {
	config.Logger.Info("[browser_session] starting browser (quick)")

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", s.cfg.Headless),
		chromedp.UserDataDir(s.cfg.UserDataDir),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.WindowSize(1360, 700),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)

	var err error
	s.allocCtx, s.allocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
	s.browserCtx, s.browserCancel = chromedp.NewContext(s.allocCtx)

	err = chromedp.Run(s.browserCtx, hideAutomation())
	if err != nil {
		config.Logger.Warn("[browser_session] failed with headless=false, retrying with headless=true",
			"error", err,
		)

		s.cleanupLocked()

		retryOpts := append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.UserDataDir(s.cfg.UserDataDir),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-dev-shm-usage", true),
			chromedp.WindowSize(1360, 700),
			chromedp.Flag("disable-blink-features", "AutomationControlled"),
		)

		s.allocCtx, s.allocCancel = chromedp.NewExecAllocator(context.Background(), retryOpts...)
		s.browserCtx, s.browserCancel = chromedp.NewContext(s.allocCtx)

		err = chromedp.Run(s.browserCtx, hideAutomation())
		if err != nil {
			s.cleanupLocked()
			return fmt.Errorf("[browser_session] failed to setup browser: %w", err)
		}

		config.Logger.Info("[browser_session] started with headless mode fallback")
	} else {
		config.Logger.Info("[browser_session] browser started successfully",
			"headless", s.cfg.Headless,
		)
	}

	return nil
}

func (s *BrowserSession) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked()
	return nil
}

func (s *BrowserSession) setupNetworkListener() {
	s.netLogs = []string{}

	chromedp.ListenTarget(s.browserCtx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *network.EventRequestWillBeSent:
			url := ev.Request.URL
			reqMethod := ev.Request.Method
			reqType := ev.Type.String()

			logEntry := fmt.Sprintf("[%s] %s type=%s", reqMethod, url, reqType)
			s.mu.Lock()
			s.netLogs = append(s.netLogs, logEntry)
			if len(s.netLogs) > 500 {
				s.netLogs = s.netLogs[len(s.netLogs)-500:]
			}
			s.mu.Unlock()

			if strings.Contains(url, "/chat/completion") {
				config.Logger.Info("[CDP_NET] completion request",
					"method", reqMethod,
					"url", url,
					"request_id", string(ev.RequestID),
				)
				s.mu.Lock()
				s.cdpStreamID = string(ev.RequestID)
				s.cdpChunks = nil
				s.cdpDone = false
				s.mu.Unlock()
			} else if strings.Contains(url, "completion") || strings.Contains(url, "chat") || strings.Contains(url, "conversation") {
				config.Logger.Info("[CDP_NET] request",
					"method", reqMethod,
					"url", url,
					"type", reqType,
				)
			}

		case *network.EventResponseReceived:
			url := ev.Response.URL
			status := ev.Response.Status
			mimeType := ev.Response.MimeType

			if strings.Contains(url, "/chat/completion") {
				config.Logger.Info("[CDP_NET] completion response",
					"url", url,
					"status", status,
					"mime", mimeType,
					"request_id", string(ev.RequestID),
				)
			} else if strings.Contains(url, "completion") || strings.Contains(url, "chat") || strings.Contains(url, "conversation") {
				config.Logger.Info("[CDP_NET] response",
					"url", url,
					"status", status,
					"mime", mimeType,
				)
			}

		case *network.EventLoadingFinished:
			s.mu.Lock()
			streamID := s.cdpStreamID
			s.mu.Unlock()

			if streamID != "" && string(ev.RequestID) == streamID {
				config.Logger.Info("[CDP_NET] completion loading finished",
					"request_id", ev.RequestID,
					"encoded_data_length", ev.EncodedDataLength,
				)
				go s.readResponseBody(s.browserCtx, ev.RequestID)
			}

		case *network.EventLoadingFailed:
			s.mu.Lock()
			streamID := s.cdpStreamID
			s.mu.Unlock()

			if streamID != "" && string(ev.RequestID) == streamID {
				config.Logger.Warn("[CDP_NET] completion loading failed",
					"request_id", ev.RequestID,
					"type", ev.Type,
					"error_text", ev.ErrorText,
				)
				s.mu.Lock()
				s.cdpDone = true
				s.mu.Unlock()
			}

		case *network.EventWebSocketCreated:
			config.Logger.Info("[CDP_NET] WebSocket created",
				"url", ev.URL,
				"requestId", ev.RequestID,
			)

		case *network.EventWebSocketFrameReceived:
			payload := ev.Response.PayloadData
			if len(payload) > 200 {
				payload = payload[:200]
			}
			config.Logger.Info("[CDP_NET] WS frame received",
				"requestId", ev.RequestID,
				"payload_len", len(ev.Response.PayloadData),
				"payload_preview", payload,
			)

		case *network.EventWebSocketFrameSent:
			payload := ev.Response.PayloadData
			if len(payload) > 200 {
				payload = payload[:200]
			}
			config.Logger.Info("[CDP_NET] WS frame sent",
				"requestId", ev.RequestID,
				"payload_len", len(ev.Response.PayloadData),
				"payload_preview", payload,
			)

		case *runtime.EventConsoleAPICalled:
			for _, arg := range ev.Args {
				if arg.Value != nil && strings.Contains(arg.Value.String(), "ds2api") {
					config.Logger.Info("[CDP_CONSOLE]",
						"type", ev.Type,
						"value", arg.Value.String(),
					)
				}
			}
		}
	})

	go func() {
		if err := chromedp.Run(s.browserCtx, network.Enable()); err != nil {
			config.Logger.Warn("[CDP_NET] Network.enable failed", "error", err)
		} else {
			config.Logger.Info("[CDP_NET] Network.enable success")
		}
	}()
}

func (s *BrowserSession) GetNetLogs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	logs := make([]string, len(s.netLogs))
	copy(logs, s.netLogs)
	return logs
}

func (s *BrowserSession) IsReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

func (s *BrowserSession) EnsureReady(ctx context.Context) error {
	config.Logger.Info("[browser_session] EnsureReady acquiring lock")
	s.mu.Lock()
	defer s.mu.Unlock()
	config.Logger.Info("[browser_session] EnsureReady lock acquired")

	if s.browserCtx == nil {
		config.Logger.Info("[browser_session] not started, attempting lazy start")
		if startErr := s.startLocked(ctx); startErr != nil {
			return fmt.Errorf("[browser_session] lazy start failed: %w", startErr)
		}
	}

	alive := s.checkAliveLocked()
	if !alive {
		config.Logger.Warn("[browser_session] browser appears dead, restarting")
		restartErr := s.restartLocked(ctx)
		if restartErr != nil {
			return fmt.Errorf("[browser_session] restart failed: %w", restartErr)
		}
	}
	config.Logger.Info("[browser_session] browser alive")

	var currentURL string
	urlCtx, urlCancel := context.WithTimeout(s.browserCtx, 10*time.Second)
	chromedp.Evaluate(`window.location.href`, &currentURL).Do(urlCtx)
	urlCancel()
	onDeepSeek := strings.Contains(currentURL, "deepseek.com")
	config.Logger.Info("[browser_session] checking navigation", "url", currentURL, "on_deepseek", onDeepSeek)

	if !onDeepSeek {
		config.Logger.Info("[browser_session] navigating to deepseek")

		navDone := make(chan error, 1)
		go func() {
			navDone <- chromedp.Run(s.browserCtx,
				navigateToDeepSeek(),
				waitForPageLoad(),
			)
		}()

		select {
		case err := <-navDone:
			if err != nil {
				return fmt.Errorf("[browser_session] navigate failed: %w", err)
			}
		case <-time.After(30 * time.Second):
			return fmt.Errorf("[browser_session] navigate timed out after 30s")
		}

		config.Logger.Info("[browser_session] navigation complete, checking page ready")

		for i := 0; i < 10; i++ {
			var pageReady bool
			readyCtx, readyCancel := context.WithTimeout(s.browserCtx, 5*time.Second)
			chromedp.Evaluate(`
				(function() {
					return document.readyState === 'complete' && 
						   document.body !== null && 
						   document.body.innerText.length > 50;
				})()
			`, &pageReady).Do(readyCtx)
			readyCancel()

			if pageReady {
				config.Logger.Info("[browser_session] navigation ready", "sec", i+1)
				break
			}

			time.Sleep(200 * time.Millisecond)
		}
	}

	config.Logger.Info("[browser_session] checking login status")
	loggedIn, err := s.checkLoginStatusLocked(ctx)
	if err != nil {
		return fmt.Errorf("[browser_session] login check failed: %w", err)
	}
	config.Logger.Info("[browser_session] login status checked", "logged_in", loggedIn)

	if !loggedIn {
		config.Logger.Info("[browser_session] session expired, re-login")
		loginErr := s.executeLogin(ctx)
		if loginErr != nil {
			return fmt.Errorf("[browser_session] re-login failed: %w", loginErr)
		}
		s.loggedInAt = time.Now()
	}

	injectErr := s.injectScriptLocked(ctx)
	if injectErr != nil {
		config.Logger.Warn("[browser_session] script injection warning", "error", injectErr)
	}

	if !s.netListenerReady {
		go func() {
			s.setupNetworkListener()
			s.mu.Lock()
			s.netListenerReady = true
			s.mu.Unlock()
		}()
	}

	s.lastUsedAt = time.Now()
	s.ready = true
	return nil
}

func (s *BrowserSession) BrowserCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.browserCtx
}

func (s *BrowserSession) AccountID() string {
	return s.account.Identifier()
}

func (s *BrowserSession) PollChunks(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	browserCtx := s.browserCtx
	s.mu.Unlock()

	if browserCtx == nil {
		return nil, fmt.Errorf("[browser_session] browser not started")
	}

	var raw string

	err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`
			(function() {
				try {
					var data = window.__ds2api_chunks || [];
				 window.__ds2api_chunks = [];
					return JSON.stringify(data);
				} catch(e) {
					return '[]';
				}
			})()
		`, &raw),
	)

	if err != nil {
		return nil, fmt.Errorf("[browser_session] poll chunks failed: %w", err)
	}

	var chunks []string
	if unmarshalErr := json.Unmarshal([]byte(raw), &chunks); unmarshalErr != nil {
		return nil, fmt.Errorf("[browser_session] poll chunks unmarshal failed (raw=%d bytes): %w", len(raw), unmarshalErr)
	}

	return chunks, nil
}

func (s *BrowserSession) ClearChunks(ctx context.Context) error {
	s.mu.Lock()
	browserCtx := s.browserCtx
	s.mu.Unlock()

	if browserCtx == nil {
		return fmt.Errorf("[browser_session] browser not started")
	}

	return chromedp.Run(browserCtx,
		chromedp.Evaluate(`window.__ds2api_chunks = []`, nil),
	)
}

func (s *BrowserSession) ResetStreamState(ctx context.Context) error {
	s.mu.Lock()
	browserCtx := s.browserCtx
	s.mu.Unlock()

	if browserCtx == nil {
		return fmt.Errorf("[browser_session] browser not started")
	}

	return chromedp.Run(browserCtx,
		chromedp.Evaluate(`
			(function() {
				window.__ds2api_chunks = [];
				window.__ds2api_done = false;
				window.__ds2api_error = null;
				window.__ds2api_status = 'idle';
			})()
		`, nil),
	)
}

func (s *BrowserSession) GetStreamStatus(ctx context.Context) string {
	s.mu.Lock()
	browserCtx := s.browserCtx
	s.mu.Unlock()

	if browserCtx == nil {
		return ""
	}

	var status string
	chromedp.Run(browserCtx, chromedp.Evaluate(`window.__ds2api_status || 'idle'`, &status))
	return status
}

func (s *BrowserSession) StoreResponse(response string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.domResponse == nil {
		s.domResponse = &DOMResponse{
			Content:   response,
			Timestamp: time.Now(),
		}
		config.Logger.Info("[browser_session] ✅ DOM response stored",
			"length", len(response),
			"preview", response[:min(100, len(response))],
		)
	}
}

func (s *BrowserSession) GetDOMResponse() (*DOMResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.domResponse != nil && s.domResponse.Content != "" {
		return s.domResponse, true
	}
	return nil, false
}

func (s *BrowserSession) ClearDOMResponse() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.domResponse = nil
}

func (s *BrowserSession) ExecuteJS(ctx context.Context, js string, result interface{}) error {
	s.mu.Lock()
	browserCtx := s.browserCtx
	s.mu.Unlock()

	if browserCtx == nil {
		return fmt.Errorf("[browser_session] browser not started")
	}

	return chromedp.Run(browserCtx,
		chromedp.Evaluate(js, result),
	)
}

func (s *BrowserSession) checkLoginStatus(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkLoginStatusLocked(ctx)
}

func (s *BrowserSession) checkLoginStatusLocked(ctx context.Context) (bool, error) {
	var hasTextarea bool
	timeoutCtx, cancel := context.WithTimeout(s.browserCtx, 10*time.Second)
	defer cancel()
	err := chromedp.Run(timeoutCtx,
		chromedp.Evaluate(`!!document.querySelector('textarea')`, &hasTextarea),
	)
	if err != nil {
		return false, err
	}
	return hasTextarea, nil
}

func (s *BrowserSession) executeLogin(ctx context.Context) error {
	email := strings.TrimSpace(s.account.Email)
	password := strings.TrimSpace(s.account.Password)

	if email == "" || password == "" {
		return fmt.Errorf("[browser_login] email or password is empty")
	}

	config.Logger.Info("[browser_login] starting unified login", "account", s.account.Email)

	err := chromedp.Run(s.browserCtx,
		chromedp.ActionFunc(func(actx context.Context) error {
			config.Logger.Info("[browser_login] navigating to deepseek")
			return nil
		}),
		chromedp.Navigate(`https://chat.deepseek.com`),
		chromedp.ActionFunc(func(actx context.Context) error {
			config.Logger.Info("[browser_login] waiting for email input")
			return nil
		}),
		chromedp.WaitVisible(`input.ds-input__input`, chromedp.ByQuery),
		chromedp.ActionFunc(func(actx context.Context) error {
			config.Logger.Info("[browser_login] clicking password button")
			return nil
		}),
		hideAutomation(),
		chromedp.Click(`.ds-sign-in-form__social-button`, chromedp.ByQuery),
		chromedp.WaitVisible(`input[type="password"]`, chromedp.ByQuery),
		chromedp.ActionFunc(func(actx context.Context) error {
			config.Logger.Info("[browser_login] filling credentials")
			return nil
		}),
		chromedp.SendKeys(`input[type="text"]`, email, chromedp.ByQuery),
		chromedp.ActionFunc(func(actx context.Context) error {
			time.Sleep(1 * time.Second)
			var emailValue string
			chromedp.Evaluate(`
				(function() {
					var input = document.querySelector('input[type="text"]');
					return input ? input.value : '';
				})()
			`, &emailValue).Do(actx)
			config.Logger.Info("[browser_login] email entered", "value_preview", emailValue[:min(20, len(emailValue))])
			return nil
		}),
		chromedp.SendKeys(`input[type="password"]`, password, chromedp.ByQuery),
		chromedp.ActionFunc(func(actx context.Context) error {
			time.Sleep(1 * time.Second)
			var pwdLength int
			chromedp.Evaluate(`
				(function() {
					var input = document.querySelector('input[type="password"]');
					return input ? input.value.length : 0;
				})()
			`, &pwdLength).Do(actx)
			config.Logger.Info("[browser_login] password entered", "length", pwdLength)
			return nil
		}),
		chromedp.ActionFunc(func(actx context.Context) error {
			config.Logger.Info("[browser_login] clicking submit")
			return nil
		}),
		chromedp.Click(`button.ds-basic-button--primary`, chromedp.ByQuery),
		chromedp.ActionFunc(func(actx context.Context) error {
			config.Logger.Info("[browser_login] submit clicked, waiting for chat page (up to 30s)")

			for i := 0; i < 30; i++ {
				time.Sleep(1 * time.Second)

				var currentURL string
				var pageText string
				var hasChatTextarea bool

				chromedp.Evaluate(`window.location.href`, &currentURL).Do(actx)
				chromedp.Evaluate(`document.body ? document.body.innerText.substring(0, 300) : 'no body'`, &pageText).Do(actx)

				onChatPage := strings.Contains(currentURL, "chat.deepseek.com") ||
					strings.Contains(currentURL, "deepseek.com/chat")

				if onChatPage {
					chromedp.Evaluate(`!!document.querySelector('textarea[placeholder*="message"]') || !!document.querySelector('textarea[placeholder*="消息"]') || !!document.querySelector('div[contenteditable="true"]')`, &hasChatTextarea).Do(actx)
				}

				if i == 0 || i == 5 || i == 10 || i == 15 || i == 20 || i == 25 || i == 29 {
					config.Logger.Info("[browser_login] post-submit status",
						"sec", i+1,
						"url", currentURL,
						"on_chat_page", onChatPage,
						"has_chat_textarea", hasChatTextarea,
						"page_preview", pageText[:min(150, len(pageText))],
					)
				}

				if onChatPage && hasChatTextarea {
					config.Logger.Info("[browser_login] chat page ready",
						"elapsed_sec", i+1,
						"url", currentURL,
					)
					return nil
				}
			}

			return fmt.Errorf("[browser_login] timeout: chat page not ready after 30s")
		}),
	)

	if err != nil {
		return fmt.Errorf("[browser_login] failed: %w", err)
	}

	return nil
}

func (s *BrowserSession) injectScriptLocked(ctx context.Context) error {
	config.Logger.Info("[browser_session] injectScriptLocked called",
		"browserCtx_nil", s.browserCtx == nil,
	)

	script, err := GetInjectionScript()
	if err != nil {
		return err
	}

	config.Logger.Info("[browser_session] injecting script into browser")

	addScriptErr := chromedp.Run(s.browserCtx,
		chromedp.ActionFunc(func(actx context.Context) error {
			_, evalErr := page.AddScriptToEvaluateOnNewDocument(script).Do(actx)
			return evalErr
		}),
	)
	if addScriptErr != nil {
		config.Logger.Warn("[browser_session] AddScriptToEvaluateOnNewDocument failed", "error", addScriptErr)
	}

	runErr := chromedp.Run(s.browserCtx,
		chromedp.Evaluate(script, nil),
	)

	if runErr != nil {
		config.Logger.Error("[browser_session] script injection failed",
			"error", runErr,
			"browserCtx_nil", s.browserCtx == nil,
		)
		return runErr
	}

	config.Logger.Info("[browser_session] script injection success")
	return nil
}

func (s *BrowserSession) checkAliveLocked() bool {
	if s.browserCtx == nil {
		return false
	}

	var result string
	aliveCtx, cancel := context.WithTimeout(s.browserCtx, 10*time.Second)
	defer cancel()
	err := chromedp.Run(aliveCtx,
		chromedp.Evaluate(`'alive'`, &result),
	)
	return err == nil && result == "alive"
}

func (s *BrowserSession) restartLocked(ctx context.Context) error {
	s.cleanupLocked()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", s.cfg.Headless),
		chromedp.UserDataDir(s.cfg.UserDataDir),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.WindowSize(1360, 700),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)

	var err error
	s.allocCtx, s.allocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
	s.browserCtx, s.browserCancel = chromedp.NewContext(s.allocCtx)

	err = chromedp.Run(s.browserCtx, hideAutomation())
	if err != nil {
		return err
	}

	err = chromedp.Run(s.browserCtx,
		navigateToDeepSeek(),
		waitForPageLoad(),
	)
	if err != nil {
		return err
	}

	time.Sleep(2 * time.Second)

	loggedIn, _ := s.checkLoginStatusLocked(ctx)
	if !loggedIn {
		err = s.executeLogin(ctx)
		if err != nil {
			return err
		}
	}

	s.ready = true
	s.loggedInAt = time.Now()
	s.lastUsedAt = time.Now()
	return nil
}

func (s *BrowserSession) cleanupLocked() {
	if s.browserCancel != nil {
		s.browserCancel()
		s.browserCancel = nil
	}
	if s.allocCancel != nil {
		s.allocCancel()
		s.allocCancel = nil
	}
	s.browserCtx = nil
	s.allocCtx = nil
	s.ready = false
}

func navigateToDeepSeek() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var currentURL string
		evalErr := chromedp.Evaluate(`window.location.href`, &currentURL).Do(ctx)
		if evalErr != nil {
			config.Logger.Warn("[browser_session] URL check failed, will navigate anyway", "err", evalErr)
		}
		config.Logger.Info("[browser_session] current URL", "url", currentURL, "eval_err", evalErr)

		if strings.Contains(currentURL, "deepseek.com") && strings.Contains(currentURL, "chat") {
			config.Logger.Info("[browser_session] already on deepseek, skipping navigation")
			return nil
		}

		config.Logger.Info("[browser_session] navigating via chromedp.Navigate with timeout")

		done := make(chan error, 1)
		go func() {
			done <- chromedp.Navigate("https://chat.deepseek.com/").Do(ctx)
		}()

		select {
		case err := <-done:
			if err != nil {
				config.Logger.Error("[browser_session] navigate error", "err", err)
			}
			return err
		case <-time.After(15 * time.Second):
			config.Logger.Error("[browser_session] navigate timed out after 15s")
			return fmt.Errorf("navigation timed out after 15s")
		}
	})
}

func waitForPageLoad() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		config.Logger.Info("[browser_session] waiting for page load")

		time.Sleep(500 * time.Millisecond)

		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		deadline := time.After(25 * time.Second)

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline:
				return fmt.Errorf("page load timeout after 25s")
			case <-ticker.C:
				var ready bool
				evalErr := chromedp.Evaluate(`
					(function() {
						if (!document.body) return false;
						if (document.querySelector('textarea')) return true;
						if (document.querySelector('[contenteditable="true"]')) return true;
						var text = document.body.innerText || '';
						return text.length > 50;
					})()
				`, &ready).Do(ctx)

				if evalErr != nil {
					continue
				}

				if ready {
					config.Logger.Info("[browser_session] page load complete")
					return nil
				}
			}
		}
	})
}

func hideAutomation() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		return chromedp.Evaluate(`
			Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
		`, nil).Do(ctx)
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *BrowserSession) readResponseBody(browserCtx context.Context, requestID network.RequestID) {
	var body []byte
	err := chromedp.Run(browserCtx,
		chromedp.ActionFunc(func(actx context.Context) error {
			var err2 error
			body, err2 = network.GetResponseBody(requestID).Do(actx)
			return err2
		}),
	)
	if err != nil {
		config.Logger.Warn("[cdp_interceptor] GetResponseBody error", "error", err)
		s.mu.Lock()
		s.cdpDone = true
		s.mu.Unlock()
		return
	}

	bodyStr := string(body)
	config.Logger.Info("[cdp_interceptor] response body read",
		"length", len(bodyStr),
	)

	lines := strings.Split(bodyStr, "\n")
	var chunks []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if dataStr == "[DONE]" {
				break
			}
			if dataStr != "" {
				chunks = append(chunks, dataStr)
			}
		}
	}

	s.mu.Lock()
	s.cdpChunks = chunks
	s.cdpDone = true
	s.mu.Unlock()

	config.Logger.Info("[cdp_interceptor] parsed chunks",
		"total", len(chunks),
		"first_preview", func() string {
			if len(chunks) > 0 {
				return chunks[0][:min(80, len(chunks[0]))]
			}
			return "none"
		}(),
	)
}

func (s *BrowserSession) PollCDPChunks() ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cdpChunks == nil {
		return nil, s.cdpDone
	}

	chunks := s.cdpChunks
	s.cdpChunks = nil
	return chunks, s.cdpDone
}
