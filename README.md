# BetterOCR

多引擎 OCR 融合工具:并发调用多个**便宜的 VLM 引擎**做识别,行级对齐后
一致的行免费通过,只有**分歧的行**打包交给一个**更强的 VLM** 看图仲裁。

强模型的成本与分歧量成正比,而融合准确率可以超过其中任何一个单引擎。

## 架构

```
图片 ─► Coordinator(并发)─► 引擎 1(便宜 VLM)─┐
                          ─► 引擎 2(便宜 VLM)─┼─► 行级对齐(Needleman-Wunsch)
                          ─► 引擎 3(便宜 VLM)─┘        │
                                              ┌────────┴────────┐
                                            一致行             分歧行
                                          直接通过     打包一次交给强 VLM 仲裁
                                          (免费)      (失败则本地择优兜底)
                                              └────────┬────────┘
                                                   最终结果
```

## 第一性原理

1. **融合必须发生在行级。** 整篇选择的准确率上限就是最好的单引擎;
   行级组合才可能超越——各引擎往往错在不同的行。
2. **一致本身就是证据。** 不同引擎的自报置信度刻度不可比,但归一化后
   完全一致的行可按独立证据合成置信度 `1-Π(1-cᵢ)`:三个引擎各 0.9 且
   一致,比单引擎 0.92 更可信。
3. **强模型成本与分歧量成正比。** 共识行零成本通过;一张图的全部分歧
   打包成**一次**仲裁调用,并附上下文行帮助强模型在图中定位。
4. **全程确定性。** 引擎按名排序、对齐平局有固定优先级、并列择优有
   字典序仲裁——同一输入永远产出同一结果。

## 快速开始

支持 OpenAI Chat Completions、OpenAI Responses 与 Anthropic Messages 三种
视觉模型 API。每个 Provider 独立配置 `base_url` / `api_key`,每个模型再选择
自己的 API 类型,因此可以同时接入官方服务、兼容网关、vLLM 与 Ollama。

全部运行参数来自 JSON 配置文件(默认 `./betterocr.json`,可用 `-config` 指定),
**不读取任何环境变量**。首次运行自动释放内置模板:

```bash
go run ./cmd/betterocr invoice.png   # 首次运行:生成 betterocr.json 后退出
```

编辑生成的 `betterocr.json`(填入各 Provider 的密钥,按需调整模型):

```json
{
	"providers": [
	  {
		"id": "openai",
		"base_url": "https://api.openai.com/v1",
		"api_key": "sk-…",
		"models": [
		  {
			"id": "gpt-4.1-mini",
			"context": 1048576,
			"alias": "GPT-4.1 mini",
			"api": "openai-responses"
		  },
		  {
			"id": "gpt-4o-mini",
			"context": 128000,
			"alias": "GPT-4o mini",
			"api": "openai-chat"
		  }
		]
	  },
	  {
		"id": "anthropic",
		"base_url": "https://api.anthropic.com/v1",
		"api_key": "sk-ant-…",
		"models": [
		  {
			"id": "claude-sonnet-4-20250514",
			"context": 200000,
			"alias": "Claude Sonnet 4",
			"api": "anthropic"
		  }
		]
	  }
	],
	"engines": ["openai/gpt-4.1-mini", "openai/gpt-4o-mini"],
	"arbiter": "anthropic/claude-sonnet-4-20250514",
  "timeout_seconds": 120,
  "serve_addr": "127.0.0.1:8787"
}
```

再次运行即可识别:

```bash
go run ./cmd/betterocr -pretty invoice.png
```

模型引用统一为 `provider/model-id`;即使模型 ID 本身含 `/` 也可直接使用。
同一引用在 `engines` 里重复出现即多路采样,
利用采样随机性制造独立证据,是最便宜的引擎组合方式。

配置文件缺少字段时,启动会按内置模板自动补全并写回;显式空值视为用户
决定,不会被改写;无法解析的文件绝不会被改动。旧版顶层
`base_url` / `api_key` 配置会自动迁移为 `default` Provider,原模型与密钥均保留。

## Web 界面(单文件二进制)

前端(React + shadcn/ui)构建后经 `go:embed` 打进二进制,最终只有一个文件,
不依赖任何 CDN,离线可用:

```bash
make build    # = cd web && npm install && npm run build,再 go build

./betterocr -serve    # 监听配置中的 serve_addr,默认 127.0.0.1:8787
```

浏览器打开 http://127.0.0.1:8787:拖拽 / 点击 / Ctrl+V 粘贴图片。顶部的
「模型配置」菜单按 Provider 展示已配置模型,可点选多个基础模型和一个仲裁
模型;结果按 文本 / 逐行(来源与置信度标注)/ 引擎对比 / JSON 四个视图展示。

- Web 模式下 `engines` / `arbiter` 是页面默认选择;浏览器会在本机记住点选结果。
- API 只接受配置文件中存在的模型引用;Provider 密钥绝不下发到浏览器,
  请求也不能覆盖端点或密钥。
- 监听地址(`serve_addr`)请保持 127.0.0.1;要暴露到公网需自行加认证层。
- 前端开发:`cd web && npm run dev`(Vite 把 /api 代理到 127.0.0.1:8787)。
- 未构建前端时 `go build` 依然可用,`-serve` 会返回一页构建指引。

## 输出

```json
{
  "text": "发票代码 044031900111\n金额 ¥1,280.00",
  "confidence": 0.9911,
  "lines": [
    {"text": "发票代码 044031900111", "confidence": 0.9984,
     "source": "consensus", "from": ["glm-4v-9b#3", "qwen2.5-vl-7b#1", "qwen2.5-vl-7b#2"]},
    {"text": "金额 ¥1,280.00", "confidence": 0.98,
     "source": "escalated", "from": ["arbiter:qwen2.5-vl-72b"]}
  ],
  "stats": {"engines": 3, "failed_engines": 0, "rows": 2,
            "consensus_rows": 1, "escalated_rows": 1, "fallback_rows": 0,
            "escalator": "arbiter:qwen2.5-vl-72b"},
  "candidates": ["…各引擎原始行级结果,含失败者的 err…"]
}
```

每行的 `source` 标记其产生方式:

| source      | 含义                                       | 成本     |
|-------------|--------------------------------------------|----------|
| `consensus` | 严格多数引擎归一化后完全一致               | 免费     |
| `escalated` | 分歧行,由仲裁 VLM 看原图裁定              | 一次打包 |
| `fallback`  | 未配置仲裁器或仲裁失败,本地确定性择优兜底 | 免费     |

## 配置与参数

命令行只保留三个开关,识别参数全部在 JSON 配置文件里:

| 命令行参数 | 说明                                                           |
|------------|----------------------------------------------------------------|
| `-config`  | 配置文件路径,默认 `betterocr.json`;不存在时自动释放内置模板  |
| `-serve`   | 以 Web 模式启动,监听配置中的 `serve_addr`                     |
| `-pretty`  | 美化 JSON 输出(CLI 模式)                                     |

| 配置字段          | 说明                                                        |
|-------------------|-------------------------------------------------------------|
| `providers`       | Provider 数组;每项包含唯一 `id`、`base_url`、`api_key` 和 `models` |
| `models[].id`     | 上游 API 使用的真实模型 ID                                 |
| `models[].context`| 上下文窗口;输出上限为 `min(8192, context/2)`               |
| `models[].alias`  | Web 菜单和识别结果使用的显示名称                           |
| `models[].api`    | `openai-chat`、`openai-responses` 或 `anthropic`           |
| `engines`         | 基础模型引用数组;重复即多路采样(CLI 模式必填)              |
| `arbiter`         | 仲裁模型引用;置空则分歧行退化为本地择优                    |
| `timeout_seconds` | 单次识别的端到端超时秒数,非正数按 120 处理                 |
| `serve_addr`      | Web 模式监听地址,如 `127.0.0.1:8787`                       |

启动时的配置文件处理:不存在 → 释放内置模板;旧结构 → 自动迁移;缺字段 →
按模板补全写回;解析或引用校验失败 → 报错且绝不改动原文件。不读取任何环境变量。

单引擎失败不影响其他引擎;全部失败时退出码 1,细节在 `candidates[].err`。

## 接入自定义引擎 / 仲裁器

实现两个接口即可(`internal/agent`、`internal/arbiter`):

```go
type Agent interface {
    Name() string
    Recognize(ctx context.Context, image []byte) (agent.Result, error)
}

type Escalator interface {
    Name() string
    Resolve(ctx context.Context, image []byte, disputes []arbiter.Dispute) ([]arbiter.Resolution, error)
}
```

## 测试

```bash
go test ./...
```

核心算法(对齐、共识、仲裁流程)用内联假实现测试,三种模型 API、HTTP 客户端
与 Web 服务(multipart 上传、模型白名单、密钥隔离、静态回退)用 `httptest`
假端点测试,不依赖网络与真实模型。Go 侧零第三方依赖,纯标准库。

## License

MIT
