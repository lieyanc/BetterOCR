// 与 Go 端 JSON 结构一一对应(见 internal/agent、internal/arbiter、internal/server)。

export interface Line {
  text: string
  confidence: number
}

export interface EngineResult {
  agent: string
  lines?: Line[]
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

export interface ServerConfig {
  engines: string[]
  arbiter: string
  base_url: string
  has_api_key: boolean
  timeout_ms: number
}

export async function fetchConfig(): Promise<ServerConfig> {
  const res = await fetch("/api/config")
  if (!res.ok) throw new Error(`获取服务端配置失败 (HTTP ${res.status})`)
  return res.json() as Promise<ServerConfig>
}

export interface OCRRequest {
  image: File
  engines: string
  arbiter: string
  baseUrl: string
  apiKey: string
  signal?: AbortSignal
}

export async function runOCR(req: OCRRequest): Promise<Final> {
  const fd = new FormData()
  fd.append("image", req.image)
  fd.append("engines", req.engines)
  // arbiter 始终提交:显式的空值表示"清空仲裁",而非回退服务端默认
  fd.append("arbiter", req.arbiter)
  if (req.baseUrl) fd.append("base_url", req.baseUrl)
  if (req.apiKey) fd.append("api_key", req.apiKey)

  const res = await fetch("/api/ocr", { method: "POST", body: fd, signal: req.signal })
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
  return res.json() as Promise<Final>
}
