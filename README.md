# BetterOCR

多引擎 OCR 融合工具:并发调用多个**便宜的 VLM 引擎**识别完整文本,后端按
中文句末标点动态切分和对齐;一致句段直接通过,有分歧的句段可自动仲裁、
由用户选择候选或编辑合并,也可稍后单独发起强模型仲裁。

强模型的成本与分歧量成正比,而融合准确率可以超过其中任何一个单引擎。

## 架构

```
图片 ─► Coordinator(并发)─► 引擎 1(便宜 VLM)─┐
                          ─► 引擎 2(便宜 VLM)─┼─► 全文动态切句 + 序列对齐
						  ─► 引擎 3(便宜 VLM)─┘        │
										      ┌────────┴────────┐
										  一致句段           争议句段
										 直接通过       自动仲裁 / 用户合并
										  (免费)       / 稍后发起仲裁
                                              └────────┬────────┘
                                                   最终结果
```

## 第一性原理

1. **模型只负责 OCR,不负责提供物理行结构。** 完整文本按中文句末标点动态
   切分;换行只视为排版空白。对齐支持 1:2 / 2:1 句段匹配,用于吸收模型漏写
   或多写句末标点造成的边界差异。
2. **判据必须可观测。** 不采信模型自报的置信度:那个数聚集在 0.9/0.95/0.98
   几个整数上,与真实正确率几乎无关,而 OCR 最容易错的地方(`0/O`、`1/l`、
   `己/已`)恰恰是模型自信读错的地方。可信度只从结构信号推导——多少个独立
   引擎逐字给出同一文本、多少个持异议、句段走了哪条路径。
3. **正文骨架只负责定位,不能掩盖 OCR 错误。** 对齐时去掉空白、标点和符号;
   判定共识时仍保留全部标点和符号,因此 `¥128` 与 `128` 必然进入争议。
4. **强模型按需使用。** 共识句段零成本通过;争议可以批量自动仲裁,也可以先
   返回前端人工合并,之后只提交选中的争议句段而无需重跑基础 OCR。
5. **全程确定性。** 引擎按名排序、对齐平局有固定优先级、代表候选取 medoid
   且平局有字典序仲裁——同一输入永远产出同一结果。

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
		"alias": "OpenAI",
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
			"api": "openai-chat-completions"
		  }
		]
	  },
	  {
		"id": "anthropic",
		"alias": "Anthropic",
		"base_url": "https://api.anthropic.com/v1",
		"api_key": "sk-ant-…",
		"models": [
		  {
			"id": "claude-sonnet-4-20250514",
			"context": 200000,
			"alias": "Claude Sonnet 4",
			"api": "anthropic-messages"
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
决定,不会被改写(唯一例外:provider 的空 `alias` 会补为模板名或 `id`);
无法解析的文件绝不会被改动。

## Web 界面(单文件二进制)

前端(React + shadcn/ui)构建后经 `go:embed` 打进二进制,最终只有一个文件,
不依赖任何 CDN,离线可用:

```bash
make build    # = cd web && npm install && npm run build,再 go build

./betterocr           # 不带图片参数即进入 Web 模式(-serve 可省略),监听配置中的 serve_addr,默认 127.0.0.1:8787
```

浏览器打开 http://127.0.0.1:8787:拖拽 / 点击 / Ctrl+V 粘贴图片。顶部的
「模型配置」菜单按 Provider 展示已配置模型,可点选多个基础模型和一个仲裁
模型;识别时会分区实时显示各基础模型与仲裁模型的思考过程和主输出。思考内容
只用于观察模型过程,不会进入 OCR 文本、候选、融合或仲裁裁定。完成后可以查看合并文本、
争议句段、全部句段、模型原文与 JSON;每个争议都能选择候选、自定义编辑或
单独发起/重新发起仲裁,处理结果会立即反馈到合并文本。

- Web 模式下 `engines` / `arbiter` 是页面默认选择;浏览器会在本机记住点选结果。
- 三种模型 API 默认都发送 `stream: true`;兼容端点若忽略该参数并返回普通 JSON,
  仍可正常解析。`POST /api/ocr/stream` 以 NDJSON 输出带 `kind: thinking|output`
  的 `delta` 与最终 `result` 事件,
  原 `POST /api/ocr` 保留为一次性 JSON 响应;`POST /api/arbitrate/stream` 用于
  不重跑基础 OCR 的独立句段仲裁。
- API 只接受配置文件中存在的模型引用;Provider 密钥绝不下发到浏览器,
  请求也不能覆盖端点或密钥。
- 监听地址(`serve_addr`)请保持 127.0.0.1;要暴露到公网需自行加认证层。
- 前端开发:`cd web && npm run dev`(Vite 把 /api 代理到 127.0.0.1:8787)。
- 未构建前端时 `go build` 依然可用,Web 模式会返回一页构建指引。

## 输出

```json
{
  "text": "发票代码 044031900111\n金额 ¥1,280.00",
  "confidence": 0.9605,
  "segments": [
    {"text": "发票代码 044031900111。", "confidence": 0.971,
     "source": "consensus", "from": ["glm-4v-9b#3", "qwen2.5-vl-7b#1", "qwen2.5-vl-7b#2"]},
    {"text": "金额 ¥1,280.00。", "confidence": 0.95,
     "source": "escalated", "from": ["arbiter:qwen2.5-vl-72b"],
     "disputed": true,
     "candidates": [{"agent": "qwen#1", "text": "金额 ¥1,280.00。"},
                    {"agent": "qwen#2", "text": "金额 1,280.00。"}]}
  ],
  "stats": {"engines": 3, "failed_engines": 0, "segments": 2,
            "consensus_segments": 1, "escalated_segments": 1, "fallback_segments": 0,
            "escalator": "arbiter:qwen2.5-vl-72b"},
  "candidates": ["…各引擎完整原文 text,含失败者的 err…"]
}
```

每个句段的 `source` 标记其产生方式:

| source      | 含义                                       | 成本     |
|-------------|--------------------------------------------|----------|
| `consensus` | 严格多数引擎归一化后完全一致               | 免费     |
| `escalated` | 争议句段,由仲裁 VLM 看原图裁定            | 一次打包 |
| `fallback`  | 等待用户处理,或仲裁失败后的确定性候选     | 免费     |

### 置信度怎么来的

引擎只返回完整纯文本,不返回任何自报指标。`confidence` 全部由结构信号推导,
因此可复现,也不额外消耗 token:

- **`consensus`** — 由逐字一致的引擎数 `k` 与有效引擎总数 `n` 决定。一致引擎
  按相关性折扣累计为独立证据(同代 VLM 会在同一个模糊字形上犯同样的错,
  一致 ≠ 独立),再按异议比例 `(n-k)/n` 折减:

  | k/n | 2/2 | 3/3 | 5/5 | 2/3 | 3/4 | 4/5 |
  |-----|-----|-----|-----|-----|-----|-----|
  |     | 0.924 | 0.971 | 0.996 | 0.770 | 0.850 | 0.890 |

  共识判据本身是 `k ≥ 2 且 2k > n`。分母取**全体有效引擎数**而非该句段的候选
  数:某个引擎在这一句段完全没有产出候选,本身就是"这段可能不存在"的证据。
- **`escalated`** — 裁定与某个引擎候选逐字吻合(拿到独立旁证)记 0.95,
  与所有候选都不同(只有仲裁器一家之言)记 0.80。
- **`fallback`** — 由候选彼此的接近程度线性映射到 `[0.30, 0.65]`;只有一个
  引擎看到的孤立句段记 0.25。上限刻意压在 0.7 以下:兜底是"没得选才选它",
  不该显示得和共识句段一样可信。

## 配置与参数

命令行只保留三个开关,识别参数全部在 JSON 配置文件里:

| 命令行参数 | 说明                                                           |
|------------|----------------------------------------------------------------|
| `-config`  | 配置文件路径,默认 `betterocr.json`;不存在时自动释放内置模板  |
| `-serve`   | 以 Web 模式启动,监听配置中的 `serve_addr`;不带图片参数时默认即 Web 模式,可省略 |
| `-pretty`  | 美化 JSON 输出(CLI 模式)                                     |

| 配置字段          | 说明                                                        |
|-------------------|-------------------------------------------------------------|
| `providers`       | Provider 数组;每项包含唯一 `id`、可选 `alias`、`base_url`、`api_key` 和 `models` |
| `providers[].alias` | Web 菜单和识别结果使用的 Provider 显示名称;留空会自动补为模板名或 `id` |
| `models[].id`     | 上游 API 使用的真实模型 ID                                 |
| `models[].context`| 上下文窗口;输出上限为 `min(8192, context/2)`               |
| `models[].alias`  | Web 菜单和识别结果使用的显示名称                           |
| `models[].api`    | `openai-chat-completions`、`openai-responses` 或 `anthropic-messages` |
| `engines`         | 基础模型引用数组;重复即多路采样(CLI 模式必填)              |
| `arbiter`         | 仲裁模型引用;置空时争议句段等待人工合并                    |
| `timeout_seconds` | 单次识别的端到端超时秒数,非正数按 120 处理                 |
| `serve_addr`      | Web 模式监听地址,如 `127.0.0.1:8787`                       |

启动时的配置文件处理:不存在 → 释放内置模板;缺字段 →
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
