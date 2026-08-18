// 与 Go 端 JSON 结构一一对应(见 internal/agent、internal/arbiter、internal/server)。

export interface EngineResult {
  agent: string
  /** 引擎的原始识别行,只有文本——置信度由融合层从结构信号推导 */
  lines?: string[]
  latency_ms: number
  err?: string
}

export type LineSource = "consensus" | "escalated" | "fallback"

export interface FinalLine {
  text: string
  confidence: number
  source: LineSource
  from: string[]
}

export interface Stats {
  engines: number
  failed_engines: number
  rows: number
  consensus_rows: number
  escalated_rows: number
  fallback_rows: number
  dropped_rows?: number
  escalator?: string
  escalation_err?: string
}

export interface Final {
  text: string
  confidence: number
  lines: FinalLine[]
  stats: Stats
  candidates: EngineResult[]
}

export type ModelAPI = "openai-chat-completions" | "openai-responses" | "anthropic-messages"

export interface ProviderModel {
  id: string
  context: number
  alias: string
  api: ModelAPI
}

export interface Provider {
  id: string
  alias: string
  base_url: string
  has_api_key: boolean
  models: ProviderModel[]
}

export interface ServerConfig {
  providers: Provider[]
  engines: string[]
  arbiter: string
  timeout_ms: number
}

export async function fetchConfig(): Promise<ServerConfig> {
  const res = await fetch("/api/config")
  if (!res.ok) throw new Error(`获取服务端配置失败 (HTTP ${res.status})`)
  return res.json() as Promise<ServerConfig>
}

export interface OCRRequest {
  image: File
  engines: string[]
  arbiter: string
  signal?: AbortSignal
}

export interface OCRDelta {
  stage: "engine" | "arbiter"
  agent: string
  text: string
}

interface OCRStreamEvent {
  type: "start" | "delta" | "result" | "error"
  stage?: OCRDelta["stage"]
  agent?: string
  text?: string
  result?: Final
  error?: string
}

export async function runOCR(
  req: OCRRequest,
  onDelta?: (delta: OCRDelta) => void,
): Promise<Final> {
  const fd = new FormData()
  fd.append("image", req.image)
  fd.append("engines", req.engines.join(","))
  // arbiter 始终提交:显式的空值表示"清空仲裁",而非回退服务端默认
  fd.append("arbiter", req.arbiter)

  const res = await fetch("/api/ocr/stream", {
    method: "POST",
    body: fd,
    signal: req.signal,
  })
  if (!res.ok) {
    let msg = `识别请求失败 (HTTP ${res.status})`
    try {
      const data = (await res.json()) as { error?: string }
      if (data.error) msg = data.error
    } catch {
      // 非 JSON 错误体,保留默认消息
    }
    throw new Error(msg)
  }
  if (!res.body) throw new Error("浏览器未提供流式响应体")
  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ""
  let result: Final | undefined

  const consume = (line: string) => {
    if (!line.trim()) return
    const event = JSON.parse(line) as OCRStreamEvent
    switch (event.type) {
      case "delta":
        if (event.stage && event.agent && event.text) {
          onDelta?.({ stage: event.stage, agent: event.agent, text: event.text })
        }
        break
      case "result":
        result = event.result
        break
      case "error":
        throw new Error(event.error || "模型流异常结束")
    }
  }

  while (true) {
    const { done, value } = await reader.read()
    buffer += decoder.decode(value, { stream: !done })
    const lines = buffer.split("\n")
    buffer = lines.pop() ?? ""
    for (const line of lines) consume(line)
    if (done) break
  }
  consume(buffer)
  if (!result) throw new Error("模型流未返回最终结果")
  return result
}
