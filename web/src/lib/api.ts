// 与 Go 端 JSON 结构一一对应(见 internal/agent、internal/arbiter、internal/server)。

export interface EngineResult {
  agent: string
  /** 模型返回的完整 OCR 文本,不要求遵循物理行 */
  text?: string
  latency_ms: number
  err?: string
}

export interface Candidate {
  agent: string
  text: string
}

export type SegmentSource = "consensus" | "escalated" | "fallback" | "user"

export interface FinalSegment {
  text: string
  confidence: number
  source: SegmentSource
  from: string[]
  disputed?: boolean
  candidates?: Candidate[]
}

export interface Stats {
  engines: number
  failed_engines: number
  segments: number
  consensus_segments: number
  escalated_segments: number
  fallback_segments: number
  dropped_segments?: number
  escalator?: string
  escalation_err?: string
}

export interface Final {
  text: string
  confidence: number
  segments: FinalSegment[]
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
  autoArbitrate: boolean
  signal?: AbortSignal
}

export interface OCRDelta {
  stage: "engine" | "arbiter"
  agent: string
  kind: "thinking" | "output"
  text: string
}

export interface Dispute {
  segment: number
  before?: string
  after?: string
  candidates: Candidate[]
}

export interface Resolution {
  segment: number
  text: string
  confidence: number
  from?: string[]
}

interface OCRStreamEvent {
  type: "start" | "delta" | "result" | "error"
  stage?: OCRDelta["stage"]
  agent?: string
  kind?: OCRDelta["kind"]
  text?: string
  result?: Final
  resolutions?: Resolution[]
  error?: string
}

export async function runOCR(
  req: OCRRequest,
  onDelta?: (delta: OCRDelta) => void,
): Promise<Final> {
  const fd = new FormData()
  fd.append("image", req.image)
  fd.append("engines", req.engines.join(","))
  // 显式空值表示清空仲裁模型,而不是回退服务端默认。
  fd.append("arbiter", req.arbiter)
  fd.append("auto_arbitrate", String(req.autoArbitrate))

  const res = await fetch("/api/ocr/stream", {
    method: "POST",
    body: fd,
    signal: req.signal,
  })
  await assertOK(res, "识别")
  return consumeStream<Final>(res, onDelta, (event) => event.result)
}

export interface ArbitrationRequest {
  image: File
  arbiter: string
  disputes: Dispute[]
  signal?: AbortSignal
}

export async function runArbitration(
  req: ArbitrationRequest,
  onDelta?: (delta: OCRDelta) => void,
): Promise<Resolution[]> {
  const fd = new FormData()
  fd.append("image", req.image)
  fd.append("arbiter", req.arbiter)
  fd.append("disputes", JSON.stringify(req.disputes))

  const res = await fetch("/api/arbitrate/stream", {
    method: "POST",
    body: fd,
    signal: req.signal,
  })
  await assertOK(res, "仲裁")
  return consumeStream<Resolution[]>(res, onDelta, (event) => event.resolutions)
}

async function assertOK(response: Response, action: string) {
  if (response.ok) return
  let message = `${action}请求失败 (HTTP ${response.status})`
  try {
    const data = (await response.json()) as { error?: string }
    if (data.error) message = data.error
  } catch {
    // 非 JSON 错误体,保留默认消息。
  }
  throw new Error(message)
}

async function consumeStream<T>(
  response: Response,
  onDelta: ((delta: OCRDelta) => void) | undefined,
  readResult: (event: OCRStreamEvent) => T | undefined,
): Promise<T> {
  if (!response.body) throw new Error("浏览器未提供流式响应体")
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ""
  let result: T | undefined
  let completed = false

  const consume = (line: string) => {
    if (!line.trim()) return
    const event = JSON.parse(line) as OCRStreamEvent
    switch (event.type) {
      case "delta":
        if (event.stage && event.agent && event.text) {
          onDelta?.({
            stage: event.stage,
            agent: event.agent,
            kind: event.kind ?? "output",
            text: event.text,
          })
        }
        break
      case "result":
        result = readResult(event)
        completed = result !== undefined
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
  if (!completed || result === undefined) throw new Error("模型流未返回最终结果")
  return result
}
