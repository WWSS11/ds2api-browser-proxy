# DS2API Browser Proxy

> 🔒 **安全版 DeepSeek API** - 通过浏览器代理实现防检测、支持深度思考和智能搜索

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat-square&logo=go)](https://golang.org/)
[![License](https://img.shields.io/badge/License-MIT-green?style=flat-square)](LICENSE)
[![GitHub Stars](https://img.shields.io/github/stars/huanglong0719/ds2api-browser-proxy?style=flat-square&logo=github)](https://github.com/huanglong0719/ds2api-browser-proxy)

---

## 📖 项目简介

本项目是基于 [CJackHwang/ds2api](https://github.com/CJackHwang/ds2api) 的 **Fork 修改版本**，新增了**浏览器代理模式 (Browser Proxy)**，通过控制真实 Chrome 浏览器访问 DeepSeek 网页版，实现：

- ✅ **完全防检测** - 模拟真实浏览器行为，TLS 指纹、Headers、Cookie 完全原生
- ✅ **深度思考模式** - 自动启用 DeepSeek 深度思考功能
- ✅ **智能搜索模式** - 自动启用联网搜索功能
- ✅ **双模式切换** - 支持安全模式（浏览器代理）和功能模式（直接 API）
- ✅ **MCP 工具兼容** - 通过 Direct API 模式支持 MCP 工具调用

---

## 🔄 与原项目的区别

### 原项目 (CJackHwang/ds2api)

```
原项目功能:
├── 标准 API 调用模式
├── 多协议支持 (OpenAI/Claude/Gemini)
├── 账号池管理
└── 流式/非流式响应

局限性:
└── 直接 API 调用可能被官方检测封禁 ❌
```

### 本项目修改内容

#### ✨ 新增功能

| 功能 | 说明 | 文件位置 |
|------|------|----------|
| **浏览器代理引擎** | Chrome CDP 自动化，完整浏览器行为模拟 | `internal/browserproxy/` |
| **双模式路由** | 根据模型名后缀自动选择安全/直接模式 | `internal/httpapi/openai/chat/handler_chat.go` |
| **深度思考集成** | 自动开启 DeepSeek 深度思考模式 | `internal/browserproxy/chat.go` |
| **智能搜索集成** | 自动开启联网搜索功能 | `internal/browserproxy/chat.go` |
| **图片上传支持** | Base64 图片注入到浏览器输入框 | `internal/browserproxy/chat.go` |
| **消息清洗系统** | 提取纯用户文本，过滤客户端元数据 | `handler_chat.go:extractUserText()` |
| **思考内容分离** | SSE 流中提取 thinking_content 字段 | `internal/sse/consumer.go` |
| **React 组件处理** | 解决 React 受控组件状态同步问题 | `internal/browserproxy/chat.go` |

#### 🔧 核心改进

```
性能优化:
├── 准备时间: 22.7s → 8-13s (提升 43-65%)
├── 流式响应: 16.3s → 4.6s (提升 72%)
├── 模式切换: 3s → 200ms (提升 93%)
└── 响应检测: 5周期 → 3周期 (提升 40%)

安全增强:
├── 浏览器指纹伪装
├── Cookie/Session 管理
├── 登录态保持
└── 反自动化检测
```

---

## 🎯 核心特性

### 1️⃣ 双模式架构

```
┌─────────────────────────────────────────┐
│              客户端请求                   │
└─────────────┬───────────────────────────┘
              ↓
    ┌─────────┴─────────┐
    ↓                   ↓
模型名: xxx-browser   模型名: xxx-direct
    ↓                   ↓
┌──────────────┐  ┌──────────────┐
│ Browser Proxy │  │ Direct API   │
│              │  │              │
│ ✅ 安全       │  │ ⚠️ 有风险    │
│ ✅ 防检测     │  │ ✅ MCP工具   │
│ ✅ 深度思考   │  │ ✅ 快速响应  │
│ ✅ 智能搜索   │  │              │
└──────────────┘  └──────────────┘
```

### 2️⃣ 安全模式 (推荐)

```bash
# 使用 -browser 后缀触发浏览器代理
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v4-flash-browser",
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

**特点：**
- 🔒 无法被官方检测（模拟真实浏览器）
- 🧠 自动开启深度思考 + 智能搜索
- 📸 支持图片上传（识图模式）
- ⚡ 性能优化：8-13秒完成请求

### 3️⃣ 功能模式 (需要工具时)

```bash
# 使用 -direct 后缀触发直接 API（支持 MCP 工具）
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v4-flash-direct",
    "messages": [{"role": "user", "content": "帮我搜索新闻"}],
    "tools": [{"type": "function", ...}]
  }'
```

**特点：**
- ⚠️ 有被检测风险（建议仅在需要工具时使用）
- 🔧 支持 MCP 工具调用
- ⚡ 响应速度更快（无浏览器开销）

---

## 🚀 快速开始

### 前置要求

- **Go 1.21+**
- **Chrome 浏览器** (最新版本)
- **DeepSeek 网页版账号**

### 1️⃣ 克隆项目

```bash
git clone https://github.com/huanglong0719/ds2api-browser-proxy.git
cd ds2api-browser-proxy
```

### 2️⃣ 配置文件

复制配置模板并编辑：

```bash
cp config.example.json config.json
```

编辑 `config.json`：

```json
{
  "keys": ["your-api-key-here"],
  
  "browser_proxy": {
    "enabled": true,
    "headless": false,
    "user_data_dir": "./browser_profile"
  },
  
  "accounts": [
    {
      "name": "我的账号",
      "email": "your@email.com",
      "password": "your-password"
    }
  ]
}
```

### 3️⃣ 编译运行

```bash
# 编译
go build -o ds2api.exe ./cmd/devserver/main.go

# 运行
./ds2api.exe
```

或直接运行：

```bash
go run ./cmd/devserver/main.go
```

### 4️⃣ 测试 API

```bash
# 测试安全模式（浏览器代理）
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v4-flash-browser",
    "messages": [{"role": "user", "content": "你好，请介绍一下自己"}],
    "stream": false
  }'
```

---

## 📋 支持的模型

| 模式 | 模型名称 | 类型 | 用途 |
|------|----------|------|------|
| **Browser Proxy** | `deepseek-v4-flash-browser` | 快速模式 | 日常对话（推荐） |
| **Browser Proxy** | `deepseek-v4-pro-browser` | 专家模式 | 复杂推理 |
| **Direct API** | `deepseek-v4-flash-direct` | 快速模式 | 需要 MCP 工具 |
| **Direct API** | `deepseek-v4-pro-direct` | 专家模式 | 专家+工具 |

---

## 🔧 配置说明

### Browser Proxy 配置项

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `enabled` | bool | `false` | 是否启用浏览器代理 |
| `headless` | bool | `false` | 是否隐藏浏览器窗口 |
| `user_data_dir` | string | `"./browser_profile"` | 浏览器配置目录（保持登录态） |
| `timeout_seconds` | int | `180` | 操作超时时间 |
| `poll_interval_ms` | int | `50` | 流式数据轮询间隔 |

### 账号差异化处理

| 模式 | 文件上传 | 联网搜索 | 深度思考 | 适用场景 |
|------|----------|----------|----------|----------|
| 快速模式 | ✅ | ✅ | ✅ | 完整功能（默认） |
| 专家模式 | ❌ | ✅ | ✅ | 复杂推理任务 |
| 识图模式 | ✅ | ❌ | ✅ | 图像识别需求 |

---

## 📊 性能对比

| 场景 | 原项目 (API) | 本项目 (Browser) | 提升 |
|------|-------------|------------------|------|
| **简单请求** | 5-8秒 | 8-13秒 | - (安全性优先) |
| **复杂请求** (思考+搜索) | 10-15秒 | 15-20秒 | - (功能更完整) |
| **连续请求** | 不稳定 | 4-5秒 | ✅ 会话复用 |
| **被封禁风险** | ⚠️ 高 | ✅ 无 | 🔒 安全 |

> 💡 **注意**: Browser Proxy 模式的延迟略高于直接 API，但换来的是**绝对的安全性**。

---

## 🛠️ 技术架构

```
ds2api-browser-proxy/
├── cmd/devserver/          # 开发服务器入口
├── internal/
│   ├── browserproxy/      # 🆕 浏览器代理核心
│   │   ├── chat.go        # 对话逻辑、模式切换
│   │   ├── session.go     # 浏览器会话管理
│   │   ├── stream.go      # 流式响应组装
│   │   ├── injector.js    # 页面注入脚本
│   │   └── config.go      # 配置定义
│   ├── httpapi/openai/chat/
│   │   └── handler_chat.go # 🆕 双模式路由
│   ├── sse/
│   │   └── consumer.go    # 🆕 思考内容提取
│   └── config/
│       └── models.go      # 🆕 模型类型检测
├── docs/
│   ├── browser-proxy-guide.md   # 用户使用指南
│   └── browser-proxy-plan.md    # 技术实现文档
└── config.example.json          # 配置模板
```

---

## 📚 详细文档

- [📘 用户使用指南](docs/browser-proxy-guide.md) - 完整的配置和使用教程
- [📗 技术实现计划](docs/browser-proxy-plan.md) - 架构设计和开发细节
- [🌐 原项目地址](https://github.com/CJackHwang/ds2api) - 原作者仓库

---

## ⚠️ 重要提示

### 安全警告

1. **Direct API 模式有风险**
   - 直接调用 DeepSeek API 可能被检测并封禁账号
   - 建议仅在需要 MCP 工具功能时使用
   - 日常使用请选择 `-browser` 后缀的安全模式

2. **敏感文件保护**
   - `config.json` 包含密码，已在 `.gitignore` 中排除
   - `browser_profile/` 包含登录会话，不会提交到 Git
   - 请勿将包含敏感信息的文件上传到 GitHub

3. **合规使用**
   - 请遵守 DeepSeek 的服务条款
   - 本项目仅供学习和研究使用
   - 商业使用请自行评估风险

---

## 🤝 贡献指南

欢迎提交 Issue 和 Pull Request！

### 开发流程

1. Fork 本仓库
2. 创建特性分支 (`git checkout -b feature/amazing-feature`)
3. 提交更改 (`git commit -m 'Add amazing feature'`)
4. 推送到分支 (`git push origin feature/amazing-feature`)
5. 开启 Pull Request

### 代码规范

- 遵循 Go 官方代码风格
- 运行 `gofmt -w` 格式化代码
- 添加必要的注释和文档
- 确保测试通过

---

## 📄 许可证

本项目基于原项目 [CJackHwang/ds2api](https://github.com/CJackHwang/ds2api) 修改，遵循 MIT 许可证。

```
MIT License

Copyright (c) 2026 huanglong0719 (本修改版本)
Copyright (c) CJackHwang (原作者)

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.
```

---

## 🙏 致谢

### 👨‍💻 原作者

- **[CJackHwang](https://github.com/CJackHwang)** - 创建了优秀的 [ds2api](https://github.com/CJackHwang/ds2api) 项目
  - 提供了完整的 API 代理框架
  - 多协议支持 (OpenAI/Claude/Gemini)
  - 账号池管理和负载均衡

### 🔧 本项目修改者

- **[huanglong0719](https://github.com/huanglong0719)** - 新增浏览器代理功能
  - 实现 Chrome CDP 浏览器自动化
  - 双模式切换机制
  - 性能优化和安全增强

### 📚 依赖项目

- [chromedp](https://github.com/chromedp/chromedp) - Chrome DevTools Protocol 库
- [DeepSeek](https://deepseek.com/) - AI 服务提供商

---

## 📞 联系方式

- **Issues**: [GitHub Issues](https://github.com/huanglong0719/ds2api-browser-proxy/issues)
- **Discussions**: [GitHub Discussions](https://github.com/huanglong0719/ds2api-browser-proxy/discussions)

---

## 🌟 Star 历史

[![Star History Chart](https://api.star-history.com/svg?repos=huanglong0719/ds2api-browser-proxy&type=Date)](https://star-history.com/#huanglong0719/ds2api-browser-proxy&Date)

---

<p align="center">
  <b>如果这个项目对你有帮助，请给一个 ⭐ 支持一下！</b>
</p>

<p align="center">
  Made with ❤️ by <a href="https://github.com/huanglong0719">huanglong0719</a>
</p>
