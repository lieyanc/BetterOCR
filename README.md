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

任何暴露 `/chat/completions` 的 OpenAI 兼容服务都能用
(OpenAI、SiliconFlow、DashScope、vLLM、Ollama……),零第三方依赖。

```bash
export OPENAI_API_KEY=sk-...                          # 本地服务可不设
export OPENAI_BASE_URL=https://api.siliconflow.cn/v1  # 或 -base-url 指定

go run ./cmd/betterocr \
  -engines qwen2.5-vl-7b,qwen2.5-vl-7b,glm-4v-9b \
  -arbiter qwen2.5-vl-72b \
  -pretty invoice.png
```

同一模型重复出现即多路采样(`qwen2.5-vl-7b,qwen2.5-vl-7b`),
利用采样随机性制造独立证据,是最便宜的引擎组合方式。

## Web 界面(单文件二进制)

前端(React + shadcn/ui)构建后经 `go:embed` 打进二进制,最终只有一个文件,
不依赖任何 CDN,离线可用:

```bash
make build    # = cd web && npm install && npm run build,再 go build

./betterocr -serve 127.0.0.1:8787 \
  -engines qwen2.5-vl-7b,qwen2.5-vl-7b,glm-4v-9b \
  -arbiter qwen2.5-vl-72b
```

浏览器打开 http://127.0.0.1:8787:拖拽 / 点击 / Ctrl+V 粘贴图片,页面上可
覆盖引擎、仲裁模型、Base URL 与 API Key;结果按 文本 / 逐行(来源与置信度
标注)/ 引擎对比 / JSON 四个视图展示。

- Web 模式下 `-engines` / `-arbiter` / `-base-url` 只是页面预填的默认值,可省略。
- 服务端的 `$OPENAI_API_KEY` 绝不下发给页面;页面留空即用服务端的 key,
  填了则覆盖本次请求。
- 监听地址请保持 127.0.0.1;要暴露到公网需自行加认证层。
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

## 参数

| 参数        | 说明                                                         |
|-------------|--------------------------------------------------------------|
| `-engines`  | 基础引擎模型,逗号分隔;重复即多路采样(CLI 必填)           |
| `-arbiter`  | 仲裁模型,应显著强于基础引擎;留空则分歧行退化为本地择优     |
| `-base-url` | API 地址;默认依次取 `$OPENAI_BASE_URL`、`$OPENAI_API_BASE`  |
| `-serve`    | 以 Web 模式启动并监听该地址,如 `127.0.0.1:8787`             |
| `-timeout`  | 单次识别的端到端超时,默认 2m                                |
| `-pretty`   | 美化 JSON 输出(CLI 模式)                                   |

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

核心算法(对齐、共识、仲裁流程)用内联假实现测试,HTTP 客户端与 Web 服务
(multipart 上传、配置覆盖、静态回退)用 `httptest` 假端点测试,不依赖网络
与真实模型。Go 侧零第三方依赖,纯标准库。

## License

MIT
