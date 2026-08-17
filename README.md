# BetterOCR

一个用 Go 编写的简单多 Agent OCR 框架：并发调度多个 OCR 引擎（Agent），
通过仲裁器（Arbiter）基于置信度与文本相似度融合结果，获得更鲁棒的识别输出。

## 架构

```
图片 ──► Coordinator（并发调度）──► Agent A / Agent B / Agent C ...
                                        │
                                        ▼
                                  Arbiter（融合仲裁）
                                        │
                                        ▼
                                   最终识别结果
```

- **Agent**：OCR 引擎抽象（`internal/agent`），实现 `Name()` 与 `Recognize()` 即可接入。
- **Coordinator**：并发调用所有注册的 Agent，单个失败不影响整体（`internal/agent`）。
- **Arbiter**：过滤低置信度结果，基于 bigram 相似度加权投票选出最佳文本（`internal/arbiter`）。

## 快速开始

```bash
go run ./cmd/betterocr -demo -pretty
```

输出（演示模式，三个 mock 引擎投票）：

```json
{
  "text": "Hello, BetterOCR!",
  "confidence": 0.92,
  "votes": {"engine-a": 1, "engine-b": 1, "engine-c": 0}
}
```

## 接入真实引擎

在 `internal/agents/` 下实现 `agent.Agent` 接口，并在 `cmd/betterocr/main.go` 中注册：

```go
reg.MustRegister(&agents.TesseractAgent{})
```

## License

MIT
