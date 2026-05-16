# 浏览器代理模式使用说明 (Browser Proxy Guide)

> 版本: 1.0
> 更新时间: 2026-05-16

---

## 一、概述

浏览器代理模式是 ds2api 的核心功能，通过控制真实 Chrome 浏览器访问 DeepSeek 网页版，绕过 API 调用限制，实现：

| 功能 | 说明 |
|------|------|
| 无限对话 | 无需 DeepSeek API Key，仅使用网页版账号 |
| 深度思考 | 自动启用「深度思考」模式 |
| 智能搜索 | 自动启用「联网搜索」模式 |
| 识图模式 | 支持发送图片（账号需开通识图权限） |
| 真实浏览器行为 | TLS 指纹、Headers、Cookie 完全原生，无法被检测 |

---

## 二、配置

### 2.1 配置文件格式

在 `config.json` 中添加以下配置：

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
      "name": "我的账号",
      "email": "your@email.com",
      "password": "your_password"
    }
  ]
}
```

### 2.2 配置项说明

| 配置项 | 类型 | 默认值 | 说明 |
|--------|------|--------|------|
| `enabled` | bool | `false` | 是否启用浏览器代理模式 |
| `headless` | bool | `false` | 是否隐藏浏览器窗口（调试时设为 `false`） |
| `user_data_dir` | string | `"./browser_profile"` | 浏览器配置文件目录（用于保持登录状态） |
| `timeout_seconds` | int | `180` | 操作超时时间（秒） |
| `poll_interval_ms` | int | `50` | 流式数据轮询间隔（毫秒） |

### 2.3 账号配置

```json
{
  "accounts": [
    {
      "name": "账号备注名称",
      "email": "deepseek@email.com",
      "password": "your_password",
      "remark": "可选备注"
    }
  ]
}
```

---

## 三、API 接口

### 3.1 接口地址

| 项目 | 值 |
|------|-----|
| 地址 | `http://localhost:8080`（本地默认） |
| 接口 | `POST /v1/chat/completions` |
| 认证 | `Authorization: Bearer <api_key>` |

### 3.2 请求头

```http
Authorization: Bearer sk-your-api-key
Content-Type: application/json
```

### 3.3 请求体格式

#### 3.3.1 纯文本消息（默认启用深度思考+智能搜索）

```json
{
  "model": "deepseek-chat",
  "messages": [
    {"role": "user", "content": "你好，请介绍一下自己"}
  ],
  "thinking": true,
  "search": true
}
```

#### 3.3.2 带图片的消息（识图模式）

```json
{
  "model": "deepseek-v4-vision",
  "messages": [
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "这张图片里有什么？"},
        {
          "type": "image_url",
          "image_url": {
            "url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
          }
        }
      ]
    }
  ],
  "thinking": true,
  "search": true
}
```

#### 3.3.3 流式响应

```json
{
  "model": "deepseek-chat",
  "messages": [{"role": "user", "content": "写一首关于春天的诗"}],
  "stream": true,
  "thinking": true,
  "search": true
}
```

### 3.4 双模式切换（重要！）

> ⚠️ **安全提示**：直接 API 调用可能被官方检测并封禁账号！

#### 模式选择方式

通过**模型名称后缀**自动切换模式：

| 模式 | 模型名示例 | 安全性 | 功能 |
|------|-----------|--------|------|
| **浏览器代理 (推荐)** | `deepseek-v4-flash-browser` | ✅ 安全 | 深度思考、智能搜索、识图 |
| **直接 API (有风险)** | `deepseek-v4-flash-direct` | ⚠️ 有风险 | 支持 MCP 工具调用 |

#### 默认行为

```
当 browser_proxy.enabled = true 时：
   - 所有请求默认走 Browser Proxy（安全模式）
   - 除非模型名包含 -direct 后缀

当 browser_proxy.enabled = false 时：
   - 所有请求走 Direct API
   - 除非模型名包含 -browser 后缀
```

#### 使用示例

```json
{
  "comment": "安全模式：使用浏览器代理（推荐日常使用）",
  "model": "deepseek-v4-flash-browser",
  "messages": [{"role": "user", "content": "你好"}]
}
```

```json
{
  "comment": "功能模式：直接 API（需要 MCP 工具时使用）",
  "model": "deepseek-v4-flash-direct",
  "messages": [{"role": "user", "content": "帮我搜索最新新闻"}],
  "tools": [...]
}
```

### 3.5 支持的模型

| 模式 | 模型 | 类型 | 说明 |
|------|------|------|------|
| Browser Proxy | `deepseek-v4-flash-browser` | 文本 | 快速模式（推荐） |
| Browser Proxy | `deepseek-v4-pro-browser` | 文本 | 专家模式 |
| Direct API | `deepseek-v4-flash-direct` | 文本 | 快速模式（支持工具） |
| Direct API | `deepseek-v4-pro-direct` | 文本 | 专家模式（支持工具） |

### 3.6 请求参数说明

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `model` | string | 必填 | 模型名称 |
| `messages` | array | 必填 | 消息列表 |
| `stream` | bool | `false` | 是否使用流式响应 |
| `thinking` | bool | `true` | 是否启用深度思考（默认开启） |
| `search` | bool | `true` | 是否启用智能搜索（默认开启） |

---

## 四、账号差异化处理

### 4.1 模式说明

| 模式 | 文件上传 | 联网搜索 | 深度思考 | 说明 |
|------|----------|----------|----------|------|
| 快速模式 | ✅ | ✅ | ✅ | 完整功能 |
| 专家模式 | ❌ | ✅ | ✅ | 无文件上传 |
| 识图模式 | ✅ | ❌ | ✅ | 图像功能 |

### 4.2 自动切换逻辑

```
客户端请求
    ↓
检测消息是否包含图片
    ↓
    ├── 有图片 → 检测账号是否支持识图模式
    │              ├── 支持 → 切换到识图模式 → 上传图片
    │              └── 不支持 → 使用快速模式
    │
    └── 无图片 → 根据 model 类型选择模式
                    ├── deepseek-v4-pro → 专家模式
                    └── 其他 → 快速模式
    ↓
自动确保深度思考+智能搜索已开启
    ↓
发送消息
```

### 4.3 默认模式行为

- **进入聊天页面时**：自动检查并开启「深度思考」和「智能搜索」
- **已开启时**：跳过，不会重复点击
- **切换模式后**：重新检查并确保这两个选项开启

---

## 五、客户端示例

### 5.1 cURL

```bash
# 纯文本消息
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-chat",
    "messages": [{"role": "user", "content": "你好"}],
    "thinking": true,
    "search": true
  }'

# 流式响应
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-chat",
    "messages": [{"role": "user", "content": "写一首诗"}],
    "stream": true
  }'
```

### 5.2 Python (OpenAI SDK)

```python
from openai import OpenAI

client = OpenAI(
    api_key="sk-your-api-key",
    base_url="http://localhost:8080/v1"
)

# 纯文本消息
response = client.chat.completions.create(
    model="deepseek-chat",
    messages=[{"role": "user", "content": "你好"}],
    thinking=True,
    search=True
)
print(response.choices[0].message.content)

# 流式响应
stream = client.chat.completions.create(
    model="deepseek-chat",
    messages=[{"role": "user", "content": "写一首诗"}],
    stream=True
)
for chunk in stream:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="")
```

### 5.3 JavaScript (Node.js)

```javascript
import OpenAI from 'openai';

const client = new OpenAI({
  apiKey: 'sk-your-api-key',
  baseURL: 'http://localhost:8080/v1'
});

// 纯文本消息
const response = await client.chat.completions.create({
  model: 'deepseek-chat',
  messages: [{ role: 'user', content: '你好' }],
  thinking: true,
  search: true
});
console.log(response.choices[0].message.content);

// 流式响应
const stream = await client.chat.completions.create({
  model: 'deepseek-chat',
  messages: [{ role: 'user', content: '写一首诗' }],
  stream: true
});

for await (const chunk of stream) {
  if (chunk.choices[0].delta.content) {
    process.stdout.write(chunk.choices[0].delta.content);
  }
}
```

---

## 六、响应格式

### 6.1 流式响应

```json
// 思考内容
{"choices":[{"delta":{"reasoning_content":"正在思考这个问题..."},"index":0}]}

// 正常内容
{"choices":[{"delta":{"content":"这是回复内容。"},"index":0}]}

// 完成
{"choices":[{"delta":{},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}
```

### 6.2 非流式响应

```json
{
  "id": "chatcmpl-browser-1234567890",
  "object": "chat.completion",
  "created": 1234567890,
  "model": "deepseek-chat",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "这是回复内容。"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 20,
    "total_tokens": 30
  }
}
```

### 6.3 思考内容

当启用深度思考时，思考内容会单独返回：

```json
{
  "choices": [{
    "message": {
      "role": "assistant",
      "content": "这是最终回复。",
      "reasoning_content": "这是AI的思考过程..."
    }
  }]
}
```

---

## 七、日志与调试

### 7.1 日志目录

日志输出到控制台，可通过以下方式查看：

```bash
# 启动服务时查看日志
.\ds2api.exe
```

### 7.2 关键日志标签

| 标签 | 说明 |
|------|------|
| `[browser_session]` | 浏览器会话管理 |
| `[chat_proxy]` | 聊天操作核心逻辑 |
| `[stream_bridge]` | 流式数据桥接 |
| `[injector]` | JS 注入相关 |

### 7.3 调试建议

1. **设置 `headless: false`**：可以看到真实的浏览器操作
2. **检查 `user_data_dir`**：确保目录有写入权限
3. **首次运行**：确保账号能正常登录 DeepSeek 网页版

---

## 八、常见问题

### 8.1 浏览器启动失败

**原因**：Chrome 进程残留或端口被占用

**解决**：
```powershell
taskkill /F /IM chrome.exe /T
.\ds2api.exe
```

### 8.2 登录失败

**原因**：账号密码错误或需要验证码

**解决**：
1. 手动打开 Chrome 访问 chat.deepseek.com
2. 完成登录和验证码
3. 删除 `user_data_dir` 目录后重试

### 8.3 识图模式不可用

**原因**：账号未开通识图权限

**解决**：系统会自动降级到快速模式，仍支持发送图片

### 8.4 响应时间过长

**可能原因**：
- 网络延迟
- 页面加载慢
- 响应内容过长

**解决**：检查网络连接，适当调整 `timeout_seconds`

---

## 九、文件结构

```
internal/
└── browserproxy/                 # 浏览器代理核心模块
    ├── session.go               # 浏览器会话管理
    ├── chat.go                  # 聊天操作（发送/模式切换）
    ├── injector.go              # JS 注入（SSE 数据捕获）
    ├── stream.go                # 流式数据桥接
    └── config.go                # 配置定义
```
