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

export type ModelAPI =
  "openai-chat-completions" | "openai-responses" | "anthropic-messages"

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

export type UserRole = "admin" | "user"

export interface User {
  id: string
  username: string
  role: UserRole
  disabled: boolean
  created_at: string
  updated_at: string
}

export interface AuthSession {
  user: User
  csrf_token: string
}

export interface TaskRecord {
  id: string
  user_id: string
  username: string
  filename: string
  status: "running" | "completed" | "failed"
  engines: string[]
  arbiter?: string
  created_at: string
  completed_at?: string
  duration_ms?: number
  result?: Final
  error?: string
}

export interface SettingsProvider extends Omit<Provider, "has_api_key"> {
  api_key: string
}

export interface AdminSettings {
  providers: SettingsProvider[]
  engines: string[]
  arbiter: string
  timeout_seconds: number
  serve_addr: string
}

let csrfToken = ""

export async function fetchSetupStatus(): Promise<{ initialized: boolean }> {
  const res = await fetch("/api/setup/status")
  await assertOK(res, "获取初始化状态")
  return res.json() as Promise<{ initialized: boolean }>
}

export async function initialize(
  username: string,
  password: string,
): Promise<AuthSession> {
  const res = await fetch("/api/setup", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, password }),
  })
  await assertOK(res, "初始化")
  const session = (await res.json()) as AuthSession
  csrfToken = session.csrf_token
  return session
}

export async function fetchSession(): Promise<AuthSession> {
  const res = await fetch("/api/auth/session")
  await assertOK(res, "获取登录状态")
  const session = (await res.json()) as AuthSession
  csrfToken = session.csrf_token
  return session
}

export async function login(
  username: string,
  password: string,
): Promise<AuthSession> {
  const res = await fetch("/api/auth/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, password }),
  })
  await assertOK(res, "登录")
  const session = (await res.json()) as AuthSession
  csrfToken = session.csrf_token
  return session
}

export async function logout(): Promise<void> {
  const res = await apiFetch("/api/auth/logout", { method: "POST" })
  await assertOK(res, "退出登录")
  csrfToken = ""
}

export async function fetchTasks(): Promise<TaskRecord[]> {
  const res = await apiFetch("/api/tasks")
  await assertOK(res, "获取任务记录")
  return res.json() as Promise<TaskRecord[]>
}

export async function fetchUsers(): Promise<User[]> {
  const res = await apiFetch("/api/admin/users")
  await assertOK(res, "获取用户")
  return res.json() as Promise<User[]>
}

export async function createUser(input: {
  username: string
  password: string
  role: UserRole
}): Promise<User> {
  return requestJSON<User>("/api/admin/users", "POST", input, "创建用户")
}

export async function updateUser(
  id: string,
  input: {
    username?: string
    password?: string
    role?: UserRole
    disabled?: boolean
  },
): Promise<User> {
  return requestJSON<User>(
    `/api/admin/users/${encodeURIComponent(id)}`,
    "PUT",
    input,
    "更新用户",
  )
}

export async function deleteUser(id: string): Promise<void> {
  const res = await apiFetch(`/api/admin/users/${encodeURIComponent(id)}`, {
    method: "DELETE",
  })
  await assertOK(res, "删除用户")
}

export async function fetchAdminSettings(): Promise<AdminSettings> {
  const res = await apiFetch("/api/admin/settings")
  await assertOK(res, "获取系统设置")
  return res.json() as Promise<AdminSettings>
}

export async function updateAdminSettings(
  settings: AdminSettings,
): Promise<AdminSettings> {
  return requestJSON<AdminSettings>(
    "/api/admin/settings",
    "PUT",
    settings,
    "保存系统设置",
  )
}

export async function fetchConfig(): Promise<ServerConfig> {
  const res = await apiFetch("/api/config")
  await assertOK(res, "获取服务端配置")
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

  const res = await apiFetch("/api/ocr/stream", {
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

  const res = await apiFetch("/api/arbitrate/stream", {
    method: "POST",
    body: fd,
    signal: req.signal,
  })
  await assertOK(res, "仲裁")
  return consumeStream<Resolution[]>(res, onDelta, (event) => event.resolutions)
}

async function requestJSON<T>(
  url: string,
  method: string,
  body: unknown,
  action: string,
): Promise<T> {
  const res = await apiFetch(url, {
    method,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  })
  await assertOK(res, action)
  return res.json() as Promise<T>
}

async function apiFetch(
  input: RequestInfo | URL,
  init: RequestInit = {},
): Promise<Response> {
  const headers = new Headers(init.headers)
  const method = (init.method ?? "GET").toUpperCase()
  if (!["GET", "HEAD", "OPTIONS"].includes(method) && csrfToken) {
    headers.set("X-CSRF-Token", csrfToken)
  }
  const response = await fetch(input, { ...init, headers })
  if (response.status === 401) {
    csrfToken = ""
    window.dispatchEvent(new CustomEvent("betterocr:unauthorized"))
  }
  return response
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
  if (!completed || result === undefined)
    throw new Error("模型流未返回最终结果")
  return result
}
