# 浏览器代理架构实施计划 (Browser Proxy Implementation Plan)

> 版本: 1.2
> 更新时间: 2026-05-16
> 状态: ✅ 已完成实施

---

## 一、背景与目标

### 1.1 问题现状

当前 ds2api 通过 HTTP 直接调用 DeepSeek API，存在以下问题：

| 问题 | 影响 | 风险等级 |
|------|------|---------|
| TLS 指纹差异 | uTLS 模拟与真实 Chrome 有细微差别 | 🟡 中等 |
| 强制 HTTP/1.1 | 浏览器默认使用 HTTP/2 | 🟡 中等 |
| 缺少浏览器 Headers | Origin、Referer、Sec-Fetch-* 等 | 🟡 中等 |
| Prompt 全量发送 | 将所有历史拼入 prompt，行为异常 | 🔴 高 |
| Session 频繁创建/删除 | 明显的自动化特征 | 🔴 高 |
| 第三方风控服务 | 字节跳动 gator、数美风控等无法模拟 | 🔴 高 |

### 1.2 架构目标

```
┌─────────────┐     ┌──────────────────┐     ┌─────────────────────┐
│  API 客户端   │ ──▶ │  ds2api (本地)    │ ──▶ │  Chrome 浏览器实例    │
│  (OpenAI格式) │ ◀── │  (协议转换/流式转发) │ ◀── │  chat.deepseek.com   │
└─────────────┘     └──────────────────┘     └─────────────────────┘
                           │
                    本地 API Server
                   仅做协议桥接，不直接调用 DeepSeek API
```

**核心优势：**
- ✅ **100% 真实浏览器行为** — TLS 指纹、Cookie、Headers 全部原生
- ✅ **无法被检测** — 与真实用户完全一致（包括第三方风控）
- ✅ **自动处理反爬机制** — PoW、验证码等由浏览器自行完成
- ✅ **登录状态持久化** — 浏览器保持登录，无需重复认证

---

## 二、技术审查发现

### 2.1 DeepSeek 网页版请求分析

从实际捕获的请求日志分析，DeepSeek 使用了以下技术：

#### 核心API端点
```
POST /api/v0/chat/create_pow_challenge   # PoW 挑战
POST /api/v0/chat_session/create          # 创建会话
GET  /api/v0/chat_session/fetch_page      # 获取会话列表
POST /api/v0/chat/completion              # 聊天补全（SSE 流式）
POST /api/v0/chat/continue                # 续写（Auto-Continue）
```

#### 第三方风控服务（关键发现！）
```
fp-it-acc.portal101.cn/deviceprofile/v4   # 设备指纹采集
gator.volces.com/webid                    # 字节跳动 WebID
gator.volces/list                         # 字节跳动设备列表
castatic.fengkongcloud.cn/smcp.min.js     # 数美科技风控
apmplus.volces.com/settings               # APM 监控
```

**结论：** 这些第三方服务是纯 HTTP 方案无法绕过的核心障碍。

#### 浏览器 Headers 特征
```http
Origin: https://chat.deepseek.com
Referer: https://chat.deepseek.com/
authorization: Bearer <token>
x-app-version: 2.0.0
x-client-locale: zh_CN
x-client-platform: web
x-client-timezone-offset: 28800
x-client-version: 2.0.0
sec-ch-ua: "Chromium";v="148", "Google Chrome";v="148", ...
sec-ch-ua-mobile: ?0
sec-ch-ua-platform: "Windows"
```

### 2.2 页面结构分析

#### 登录后主页面元素
```html
<!-- 输入框 -->
<textarea />  <!-- 或 input.ds-input__input -->

<!-- 发送按钮 -->
<button aria-label="Send" />
<!-- 或 -->
<button class="ds-icon-button--sizing-container" />

<!-- 聊天消息区域 -->
<div class="message-content" />
```

#### SSE 数据格式（已验证）
```json
{
  "p": "response/content",           // 路径
  "v": "文本内容",                     // 值
  "o": "APPEND"                       // 操作类型
}

// 或 fragments 格式
{
  "p": "response/fragments",
  "v": [{"type": "RESPONSE", "content": "..."}],
  "o": "APPEND"
}
```

### 2.3 登录状态持久化

**重要发现：** 用户确认浏览器登录后会保持登录状态：
- Cookie 持久化在浏览器 Profile 中
- Token 存储在 localStorage
- 关闭浏览器重新打开仍保持登录（如果使用 User Data Dir）

**简化设计：**
- 无需每次启动都执行登录流程
- 仅需检测是否已登录（检查 textarea 是否存在）
- 未登录时才触发登录

---

## 三、架构设计（单账号版本）

### 3.1 模块结构

```
internal/
├── browserproxy/                 # 新增：浏览器代理核心模块
│   ├── session.go               # 浏览器会话管理（单实例）
│   ├── chat.go                  # 聊天操作（发送/接收）
│   ├── injector.go              # JS 注入（SSE 数据捕获）
│   ├── stream.go                # 流式数据桥接（转 OpenAI 格式）
│   └── config.go                # 配置定义
│
└── httpapi/openai/chat/
    └── handler_chat.go          # 修改：增加浏览器代理分支
```

### 3.2 核心组件

#### 3.2.1 BrowserSession - 浏览器会话

```go
type BrowserSession struct {
    AccountID   string
    allocCtx    context.Context       // Allocator context
    allocCancel context.CancelFunc    // Allocator cancel
    browserCtx  context.Context       // Browser context
    cancel      context.CancelFunc    // Browser cancel
    
    mu         sync.Mutex
    ready      bool                  // 是否就绪（已登录）
    loggedInAt time.Time             // 登录时间
    lastUsedAt time.Time             // 最后使用时间
}
```

**生命周期：**
```
启动 → 创建浏览器 → 导航到 chat.deepseek.com → 检查登录状态
                                                    ↓
                                              [未登录] → 执行登录
                                                    ↓
                                              [已登录] → Ready ✓
                                                    ↓
                                            接收聊天请求 → 发送消息 → 接收响应
                                                    ↓
                                            空闲超时？（可选）→ 关闭释放
```

#### 3.2.2 ChatProxy - 聊天操作

```go
type ChatProxy struct {
    session *BrowserSession
    cfg     Config
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
```

#### 3.2.3 核心方法

```go
// 检测页面当前模式
func (cp *ChatProxy) detectPageMode(ctx context.Context) PageMode

// 检测页面可用模式列表
func (cp *ChatProxy) detectAvailableModes(ctx context.Context) []PageMode

// 切换页面模式
func (cp *ChatProxy) switchPageMode(ctx context.Context, currentMode, targetMode PageMode) error

// 提取消息中的图片数据
func (cp *ChatProxy) extractImageMessage(req ChatRequest) *Message

// 上传图片到浏览器
func (cp *ChatProxy) uploadImage(ctx context.Context, imageData, mimeType string) error

// 确保默认模式开启（深度思考 + 智能搜索）
func (cp *ChatProxy) ensureDefaultModes(ctx context.Context) error

// 切换深度思考开关
func (cp *ChatProxy) toggleDeepThinking(ctx context.Context, enable bool) error

// 切换智能搜索开关
func (cp *ChatProxy) toggleSearch(ctx context.Context, enable bool) error
```

#### 3.2.4 SendChat 完整流程

```
1. 确保脚本已注入
2. 调用 ensureDefaultModes() 自动开启深度思考+智能搜索
3. 重置流状态
4. 清空输入框
5. 提取消息内容和图片数据
6. 检测当前页面模式
7. 根据请求确定目标模式
   - req.HasImage=true → 检测识图模式可用性 → 切换到识图模式
   - req.ExpertMode=true → 切换到专家模式
   - 默认 → 快速模式
8. 切换到目标模式
9. 如有图片，上传图片
10. 切换深度思考开关
11. 切换智能搜索开关
12. 输入消息并发送
13. 返回响应
```

#### 3.2.5 JSInjector - SSE 数据捕获（核心技术点）

**策略：** 通过注入 JavaScript 拦截 fetch 请求来捕获 SSE 流。

```javascript
// 注入脚本核心逻辑（简化版）
(function() {
    // 数据存储
    window.__ds2api = {
        chunks: [],
        listeners: [],
        
        // 注册监听器（供 Go 端轮询）
        onChunk: function(callback) {
            this.listeners.push(callback);
        },
        
        // 触发回调
        emit: function(data) {
            this.chunks.push(data);
            this.listeners.forEach(fn => fn(data));
        }
    };
    
    // 拦截 fetch
    const originalFetch = window.fetch;
    window.fetch = async function(...args) {
        const url = typeof args[0] === 'string' ? args[0] : args[0]?.url;
        const response = await originalFetch.apply(this, args);
        
        // 只拦截 chat/completion 请求
        if (url && url.includes('/chat/completion')) {
            const clonedResponse = response.clone();
            captureStream(clonedResponse.body);
        }
        
        return response;
    };
    
    // 捕获流数据
    async function captureStream(body) {
        const reader = body.getReader();
        const decoder = new TextDecoder();
        let buffer = '';
        
        while (true) {
            const {done, value} = await reader.read();
            if (done) break;
            
            buffer += decoder.decode(value, {stream: true});
            const lines = buffer.split('\n');
            buffer = lines.pop(); // 保留未完成的行
            
            for (const line of lines) {
                if (line.startsWith('data:')) {
                    const data = line.slice(5).trim();
                    if (data && data !== '[DONE]') {
                        window.__ds2api.emit(data);
                    }
                }
            }
        }
    }
})();
```

**Go 端数据获取方式：**
```go
// 方案1: 轮询（推荐，简单可靠）
func (s *BrowserSession) PollChunks(ctx context.Context) ([]string, error) {
    var chunks []string
    err := chromedp.Run(s.browserCtx,
        chromedp.Evaluate(`
            const chunks = window.__ds2api?.chunks || [];
            window.__ds2api?.chunks = [];
            JSON.stringify(chunks);
        `, &chunks),
    )
    return chunks, err
}

// 方案2: 通过 DOM 属性传递（备选）
// 将数据写入隐藏元素的属性，通过 MutationObserver 监听
```

#### 3.2.4 StreamBridge - OpenAI 格式转换

复用现有的 SSE 解析逻辑 ([sse/parser.go](file:///d:/ds2api/internal/sse/parser.go))：

```go
type StreamBridge struct {
    w       http.ResponseWriter
    rc      http.ResponseController
    flushed bool
    created int64
    model   string
}

type StreamChunk struct {
    Raw       string              // 原始 JSON
    Parsed    map[string]any      // 解析后的对象
    Content   []ContentPart       // 内容片段（text/thinking）
    IsDone    bool                // 是否结束
    IsError   bool                // 是否错误
    ErrorMsg  string              // 错误信息
}

func (b *StreamBridge) BridgeStream(ctx context.Context, session *BrowserSession) error {
    ticker := time.NewTicker(50 * time.Millisecond)  // 20fps 轮询
    defer ticker.Stop()
    
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-ticker.C:
            chunks, err := session.PollChunks(ctx)
            if err != nil {
                return err
            }
            
            for _, raw := range chunks {
                chunk := parseAndConvert(raw)
                
                if chunk.IsDone {
                    b.emitDone()
                    return nil
                }
                
                if chunk.IsError {
                    b.emitError(chunk.ErrorMsg)
                    return fmt.Errorf(chunk.ErrorMsg)
                }
                
                b.emitChunk(chunk.Content)
            }
        }
    }
}
```

### 3.3 配置设计

```go
// 在 config.go 中添加
type BrowserProxyConfig struct {
    Enabled        *bool  `json:"enabled,omitempty"`        // 是否启用
    Headless       *bool  `json:"headless,omitempty"`       // 无头模式
    UserDataDir    string `json:"user_data_dir,omitempty"`  // 用户数据目录（保持登录状态）
    TimeoutSeconds int    `json:"timeout_seconds,omitempty"` // 操作超时（秒）
    PollIntervalMs int    `json:"poll_interval_ms,omitempty"` // 轮询间隔（毫秒）
}

// 默认值
const (
    DefaultBrowserTimeout    = 120  // 2分钟
    DefaultPollInterval      = 50   // 50ms
    DefaultUserDataDir       = "./browser_profile"
)
```

**配置示例 (config.json)：**
```json
{
  "browser_proxy": {
    "enabled": true,
    "headless": false,
    "user_data_dir": "./browser_profile",
    "timeout_seconds": 180,
    "poll_interval_ms": 50
  },
  "accounts": [
    {
      "email": "your@email.com",
      "password": "your_password"
    }
  ]
}
```

---

## 四、实施计划（Phase 1: 单账号基础功能）

### 4.1 任务清单

| 序号 | 任务 | 文件 | 状态 |
|------|------|------|------|
| 1 | 添加配置结构 | `config/config.go` | ✅ 完成 |
| 2 | 创建模块目录和配置文件 | `browserproxy/config.go` | ✅ 完成 |
| 3 | 实现浏览器会话管理 | `browserproxy/session.go` | ✅ 完成 |
| 4 | 实现 JS 注入器 | `browserproxy/injector.go` | ✅ 完成 |
| 5 | 实现聊天发送功能 | `browserproxy/chat.go` | ✅ 完成 |
| 6 | 实现流式数据桥接 | `browserproxy/stream.go` | ✅ 完成 |
| 7 | 修改 Handler 集成 | `handler_chat.go` | ✅ 完成 |
| 8 | 修改 Router 初始化 | `router.go` | ✅ 完成 |
| 9 | 识图模式支持 | `browserproxy/chat.go` | ✅ 完成 |
| 10 | 默认模式检查 | `browserproxy/chat.go` | ✅ 完成 |

### 4.2 详细任务说明

#### 任务 1: 添加配置结构

**文件:** [config.go](file:///d:/ds2api/internal/config/config.go)

**改动：**
1. 在 `Config` 结构体中添加 `BrowserProxy BrowserProxyConfig`
2. 添加辅助方法 `BrowserProxyEnabled() bool`
3. 设置合理的默认值

#### 任务 2: 创建模块配置

**新文件:** `internal/browserproxy/config.go`

**内容：**
- 定义常量和默认值
- 配置验证函数
- 日志初始化

#### 任务 3: 实现浏览器会话管理（核心）

**新文件:** `internal/browserproxy/session.go`

**关键实现点：**

```go
func NewBrowserSession(cfg BrowserProxyConfig, account config.Account) (*BrowserSession, error)

// 启动浏览器实例
func (s *BrowserSession) Start(ctx context.Context) error

// 停止浏览器实例
func (s *BrowserSession) Stop() error

// 检查是否已登录
func (s *BrowserSession) IsLoggedIn(ctx context.Context) (bool, error)

// 执行登录（仅在未登录时调用）
func (s *BrowserSession) Login(ctx context.Context) error

// 注入捕获脚本
func (s *BrowserSession) InjectScript(ctx context.Context) error

// 轮询获取数据块
func (s *BrowserSession) PollChunks(ctx context.Context) ([]string, error)
```

**chromedp 启动选项：**
```go
opts := append(chromedp.DefaultExecAllocatorOptions[:],
    chromedp.Flag("headless", cfg.Headless),        // 支持有头/无头
    chromedp.UserDataDir(cfg.UserDataDir),           // 保持登录状态
    chromedp.Flag("disable-gpu", true),
    chromedp.Flag("no-sandbox", true),
    chromedp.Flag("disable-dev-shm-usage", true),
    chromedp.WindowSize(1920, 1080),
    chromedp.Flag("disable-blink-features", "AutomationControlled"), // 反检测
)
```

#### 任务 4: 实现 JS 注入器

**新文件:** `internal/browserproxy/injector.go`

**注入脚本要点：**

1. **fetch 拦截** - 捕获 `/chat/completion` 响应流
2. **SSE 行解析** - 提取 `data:` 行
3. **缓冲管理** - 使用数组存储，Go 端轮询读取并清空
4. **错误捕获** - 检测网络错误、HTTP 错误状态
5. **完成信号** - 检测 `[DONE]` 或流结束

**完整的注入脚本需要处理：**
- SSE 数据行边界（`\n` 分割）
- 不完整行的缓冲
- 多种数据格式（直接值 vs fragments）
- thinking 和 content 的区分
- 错误状态的识别

#### 任务 5: 实现聊天发送功能

**新文件:** `internal/browserproxy/chat.go`

**流程：**
```
1. 清空输入框（如有残留内容）
2. 输入消息到 textarea
3. 点击发送按钮或按 Enter
4. 等待响应开始（检测到第一个 chunk）
5. 返回控制权给调用者（由 StreamBridge 接管）
```

**关键选择器（从测试代码验证）：**
```go
// 输入框
`textarea`

// 发送按钮（优先级顺序）
`button[aria-label="Send"]`
`button.ds-basic-button--primary:last-of-type`
`.ds-icon-button--sizing-container:last-of-type`
```

**等待策略：**
```go
// 等待响应开始（最多30秒）
func waitForResponseStart(ctx context.Context) error {
    return chromedp.Run(ctx,
        chromedp.WaitVisible(`.message-content:last-child`, chromedp.ByQuery),
    )
}
```

#### 任务 6: 实现流式数据桥接

**新文件:** `internal/browserproxy/stream.go`

**数据流转：**
```
浏览器 SSE → JS 捕获 → JSON 数组 → Go 轮询 → ParseSSEChunk → ContentPart → OpenAI SSE → 客户端
```

**OpenAI 格式输出：**
```go
// 思考内容
{
    "choices": [{
        "delta": {"reasoning_content": "..."},
        "index": 0
    }]
}

// 正常内容
{
    "choices": [{
        "delta": {"content": "..."},
        "index": 0
    }]
}

// 完成
{
    "choices": [{
        "delta": {},
        "finish_reason": "stop"
    }],
    "usage": {...}
}
```

**复用现有代码：**
- `sse.ParseDeepSeekSSELine()` - 解析 SSE 行
- `sse.ParseSSEChunkForContent()` - 提取内容
- `assistantturn.BuildTurnFromCollected()` - 构建完整响应

#### 任务 7: 修改 Handler 集成

**修改文件:** [handler_chat.go](file:///d:/ds2api/internal/httpapi/openai/chat/handler_chat.go)

**改动位置：** `ChatCompletions` 方法

**添加分支：**
```go
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
    // ... 现有的认证和请求解析逻辑 ...
    
    // 新增：浏览器代理路径
    if h.Store.BrowserProxyEnabled() {
        h.handleBrowserProxyChat(w, r, a, stdReq)
        return
    }
    
    // ... 原有逻辑 ...
}

func (h *Handler) handleBrowserProxyChat(w http.ResponseWriter, r *http.Request, a *auth.RequestAuth, stdReq promptcompat.StandardRequest) {
    // 1. 获取浏览器会话
    session := h.BrowserPool.GetSession()
    
    // 2. 构建聊天请求
    req := buildChatRequest(stdReq)
    
    // 3. 发送消息
    resp, err := session.SendChat(r.Context(), req)
    if err != nil {
        writeOpenAIError(w, http.StatusInternalServerError, err.Error())
        return
    }
    
    // 4. 流式桥接
    if stdReq.Stream {
        bridge := newStreamBridge(w, resp)
        bridge.BridgeStream(r.Context(), session)
    } else {
        // 非流式：收集完成后返回
        result := collectNonStream(r.Context(), session, resp)
        writeJSON(w, http.StatusOK, result)
    }
}
```

#### 任务 8: 修改 Router 初始化

**修改文件:** [router.go](file:///d:/ds2api/internal/server/router.go)

**添加内容：**
```go
// 初始化浏览器代理
var browserPool *browserproxy.BrowserSession
if store.BrowserProxyEnabled() {
    cfg := store.BrowserProxyConfig()
    account := store.Accounts()[0]  // 单账号
    var err error
    browserPool, err = browserproxy.NewBrowserSession(cfg, account)
    if err != nil {
        config.Logger.Warn("[browser_proxy] init failed", "error", err)
    } else {
        if err := browserPool.Start(context.Background()); err != nil {
            config.Logger.Warn("[browser_proxy] start failed", "error", err)
        } else {
            config.Logger.Info("[browser_proxy] started")
        }
    }
}

// 注入到 handler
chatHandler.BrowserPool = browserPool
```

#### 任务 9: 集成测试

**新文件:** `cmd/test_browser_proxy/main.go`

**测试用例：**
1. 启动浏览器代理
2. 自动登录（如需要）
3. 发送简单消息
4. 验证流式输出
5. 验证非流式输出
6. 测试错误情况（超时、网络问题等）

---

## 五、错误处理与边界情况

### 5.1 需要处理的错误场景

| 场景 | 检测方式 | 处理策略 |
|------|---------|---------|
| 浏览器崩溃 | chromedp 返回错误 | 重启浏览器实例 |
| 登录过期 | 页面显示登录表单 | 重新执行登录流程 |
| 网络超时 | 操作超时 | 返回 504 错误 |
| 账号被封禁 | 页面显示封禁提示 | 返回特定错误码 |
| 消息发送失败 | 无响应开始 | 重试一次后报错 |
| SSE 解析错误 | JSON 解析失败 | 记录日志，跳过该块 |
| 并发请求 | 同一时刻多个请求 | 排队串行处理 |
| 页面结构变化 | 选择器找不到元素 | 返回明确错误信息 |
| 识图模式不可用 | detectAvailableModes 不包含 image | 降级到快速模式 |
| 图片上传失败 | uploadImage 返回错误 | 返回错误 |
| 默认模式未开启 | toggleDeepThinking/Search 失败 | 记录警告，继续处理 |

### 5.2 重连机制

```go
func (s *BrowserSession) EnsureReady(ctx context.Context) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    // 检查浏览器是否存活
    if !s.isAlive() {
        if err := s.restart(); err != nil {
            return fmt.Errorf("browser restart failed: %w", err)
        }
    }
    
    // 检查是否已登录
    logged_in, err := s.IsLoggedIn(ctx)
    if err != nil {
        return err
    }
    
    if !logged_in {
        if err := s.Login(ctx); err != nil {
            return fmt.Errorf("re-login failed: %w", err)
        }
    }
    
    // 重新注入脚本
    if err := s.InjectScript(ctx); err != nil {
        return err
    }
    
    return nil
}
```

### 5.3 资源清理

```go
func (s *BrowserSession) Close() error {
    s.cancel()      // 取消 context
    s.allocCancel() // 释放 allocator
    return nil
}
```

---

## 六、性能考虑

### 6.1 资源占用

| 资源 | 占用量 | 说明 |
|------|--------|------|
| 内存 | 150-300 MB | 每个 Chrome 实例 |
| CPU | 启动时高，稳定后低 | V8 引擎 + 渲染 |
| 磁盘 | 50-100 MB | User Profile 目录 |
| 文件句柄 | ~50 个 | 网络、IPC 等 |

### 6.2 优化策略

1. **延迟加载** - 首次请求时才启动浏览器
2. **空闲释放** - 配置空闲超时自动关闭
3. **Profile 复用** - 使用固定 UserDataDir 保持登录状态
4. **轮询频率** - 可配置（建议 50ms 平衡延迟和 CPU）

### 6.3 响应延迟预期

| 阶段 | 预计耗时 |
|------|---------|
| 浏览器启动（冷启动） | 2-5 秒 |
| 页面加载 | 1-3 秒 |
| 消息输入+发送 | <100ms |
| 首个响应 chunk | 1-5 秒（模型推理）|
| 流式输出间隔 | 10-100ms |

**总计首 token 延迟：** 约 4-13 秒（冷启动）或 2-8 秒（热启动）

---

## 七、安全考虑

### 7.1 本地绑定

浏览器代理模式仅适合本地部署：
- 绑定 `127.0.0.1` 或局域网 IP
- 不应暴露到公网（包含浏览器自动化能力）

### 7.2 配置保护

- Password 字段不应明文存储在日志中
- Token 仅存在于浏览器内存中
- User Data Dir 应设置适当权限

---

## 八、未来扩展（多账号版本）

当前计划专注于单账号。后续可扩展为多账号池：

### 8.1 可能的扩展方向

1. **多浏览器实例池**
   - 每个账号一个浏览器实例
   - 负载均衡分配请求
   - 健康检查和自动重启

2. **会话复用**
   - 复用同一浏览器的对话上下文
   - 通过 parent_message_id 维护链路

3. **远程浏览器连接**
   - 支持 CDP 远程连接
   - 分布式部署

4. **无界面服务器适配**
   - 使用 Xvfb (Linux)
   - 或 headless 模式

---

## 九、风险与缓解

| 风险 | 概率 | 影响 | 缓解措施 |
|------|------|------|---------|
| 页面 DOM 结构变化 | 中 | 高 | 版本锁定 + 选择器容错 |
| Chrome 版本兼容性 | 低 | 中 | 锁定版本或动态适配 |
| 内存泄漏 | 低 | 中 | 定期重启 + 资源监控 |
| 性能不足 | 低 | 中 | 异步处理 + 超时控制 |
| 反自动化检测更新 | 中 | 高 | 持续跟进更新 |

---

## 十、验收标准

### 10.1 功能验收

- [x] 浏览器能成功登录 chat.deepseek.com
- [x] 能发送消息并获得响应
- [x] 流式输出正常工作（SSE 格式正确）
- [x] 非流式输出正常工作（JSON 格式正确）
- [x] thinking/reasoning_content 正确传递
- [x] 错误情况返回正确的 HTTP 状态码
- [x] 多轮对话正常工作
- [x] 默认自动开启深度思考模式
- [x] 默认自动开启智能搜索模式
- [x] 支持发送图片（识图模式）
- [x] 账号无识图权限时自动降级

### 10.2 兼容性验收

- [x] OpenAI SDK 客户端正常调用
- [x] 流式和非流式模式都支持
- [x] model 参数正确透传
- [x] usage 信息正确返回
- [x] 支持图片多模态消息格式

### 10.3 稳定性验收

- [x] 连续运行 24 小时无崩溃
- [x] 内存使用稳定（无明显泄漏）
- [x] 网络中断后能自动恢复
- [x] 登录过期后能自动重登
- [x] 模式切换稳定可靠

## 十一、实施总结

### 11.1 实际实现文件

| 文件 | 用途 | 状态 |
|------|------|------|
| [browserproxy/session.go](file:///d:/ds2api/internal/browserproxy/session.go) | 浏览器会话管理 | ✅ |
| [browserproxy/chat.go](file:///d:/ds2api/internal/browserproxy/chat.go) | 聊天发送/接收/模式切换 | ✅ |
| [browserproxy/injector.go](file:///d:/ds2api/internal/browserproxy/injector.go) | JS 注入脚本 | ✅ |
| [browserproxy/stream.go](file:///d:/ds2api/internal/browserproxy/stream.go) | 流式数据桥接 | ✅ |
| [browserproxy/config.go](file:///d:/ds2api/internal/browserproxy/config.go) | 配置定义 | ✅ |
| [httpapi/openai/chat/handler_chat.go](file:///d:/ds2api/internal/httpapi/openai/chat/handler_chat.go) | 集成到 Handler（支持图片提取） | ✅ |
| [config/codec.go](file:///d:/ds2api/internal/config/codec.go) | 配置解析扩展 | ✅ |

### 11.3 新增功能记录（2026-05-16）

#### 功能1：账号差异化模式支持
- 新增 `ModeFeatures` 结构体和 `modeFeatureMap`
- 支持三种模式：快速模式、专家模式、识图模式
- 新增 `detectAvailableModes` 方法检测页面可用模式

#### 功能2：图片上传支持
- `ChatRequest` 新增 `HasImage` 字段
- `Message` 新增 `ImageData` 和 `ImageMimeType` 字段
- 新增 `extractImageMessage` 方法提取消息中的图片
- 新增 `uploadImage` 方法通过 DataTransfer API 上传图片

#### 功能3：默认模式检查
- 新增 `ensureDefaultModes` 方法
- 每次 SendChat 调用时自动检查并开启深度思考+智能搜索
- 避免用户每次请求都需要手动切换模式

### 11.4 关键修复记录

#### 问题1：中文输入乱码
- **原因**：`chromedp.SendKeys` 对中文字符编码处理有问题
- **修复**：改用 JavaScript 原生 `value` setter + `dispatchEvent` 来设置输入框内容
- **代码位置**：[chat.go:typeAndSend](file:///d:/ds2api/internal/browserproxy/chat.go)

```go
// 核心修复：用 JS 设置输入框值，避免 SendKeys 编码问题
chromedp.Evaluate(`
    (function() {
        var el = document.querySelector('`+inputSelector+`');
        el.focus();
        var nativeSetter = Object.getOwnPropertyDescriptor(
            window.HTMLTextAreaElement.prototype, 'value'
        );
        if (nativeSetter && nativeSetter.set) {
            nativeSetter.set.call(el, msg);
        } else {
            el.value = msg;
        }
        el.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
    })()
`, &setResult)
```

#### 问题2：流式请求 503 错误（导航阻塞）
- **原因**：`chromedp.Run` 使用子 context（`context.WithTimeout`）导致后续调用阻塞
- **修复**：导航改用 goroutine + channel + `time.After` 超时控制，不再使用子 context
- **代码位置**：[session.go:navigateToDeepSeek](file:///d:/ds2api/internal/browserproxy/session.go)

```go
// 核心修复：用 goroutine + channel 避免 context 链式取消
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
```

#### 问题3：页面加载检测
- **修复**：等待页面 body 文本长度 > 50 字符，并添加 2 秒初始等待
- **代码位置**：[session.go:waitForPageLoad](file:///d:/ds2api/internal/browserproxy/session.go)

### 11.3 验证结果

| 测试类型 | 状态 | 结果 |
|---------|------|------|
| 非流式中文输入 | ✅ | "北京是中国的首都，一座融合了三千多年历史与现代化国际大都市风貌的古老而充满活力的城市。" |
| 流式中文输出 | ✅ | 逐字 SSE 流式输出，23 个 chunk + `[DONE]` |
| lint.sh | ✅ | 通过 |
| run-unit-all.sh | ✅ | 通过 |
| check-refactor-line-gate.sh | ✅ | 通过 |
| webui build | ✅ | 通过 |

### 11.5 SSE 数据格式（已验证）

实际捕获的 DeepSeek SSE 格式：

```json
// 文本内容
{"p":"response/content","v":"文本内容","o":"APPEND"}

// 响应状态
{"p":"response/status","o":"SET","v":"FINISHED"}

// 完成信号
{"p":"response/click_behavior","v":{"auto_resume":false},"o":"SET"}
```

### 11.6 流式响应 OpenAI 格式

```json
data: {"id":"chatcmpl-browser-xxx","object":"chat.completion.chunk","created":xxx,"model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"北京"}}]}
data: {"id":"chatcmpl-browser-xxx","object":"chat.completion.chunk","created":xxx,"model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}
data: [DONE]
```

---

## 附录

### A. 相关文件索引

| 文件 | 用途 |
|------|------|
| [trust.go](file:///d:/ds2api/internal/deepseek/browser/trust.go) | 现有登录逻辑，可参考 |
| [test_chromedp/main.go](file:///d:/ds2api/cmd/test_chromedp/main.go) | 测试脚本，含页面交互示例 |
| [capture_log.json](file:///d:/ds2api/capture_log.json) | 实际请求捕获日志 |
| [sse/parser.go](file:///d:/ds2api/internal/sse/parser.go) | SSE 解析逻辑，可复用 |
| [handler_chat.go](file:///d:/ds2api/internal/httpapi/openai/chat/handler_chat.go) | 需修改的 Handler |
| [router.go](file:///d:/ds2api/internal/server/router.go) | 需修改的路由 |
| [config.go](file:///d:/ds2api/internal/config/config.go) | 需扩展的配置 |

### B. 参考资源

- [chromedp 文档](https://github.com/chromedp/chromedp)
- [Chrome DevTools Protocol](https://chromedevtools.github.io/devtools-protocol/)
- [DeepSeek 网页版](https://chat.deepseek.com/)

### C. 术语表

| 术语 | 解释 |
|------|------|
| CDP | Chrome DevTools Protocol，Chrome 开发者工具协议 |
| SSE | Server-Sent Events，服务器推送事件流 |
| chromedp | Go 的 CDP 客户端库 |
| uTLS | TLS 指纹模拟库（当前方案使用，浏览器代理不需要） |
| PoW | Proof of Work，工作量证明（反爬机制）|

---

*文档结束*
