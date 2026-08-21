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
  | "openai-chat-completions"
  | "openai-responses"
  | "anthropic-messages"

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
  engine_timeout_ms: number
  arbiter_timeout_ms: number
  engine_max_attempts: number
  arbiter_max_attempts: number
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
  engine_timeout_seconds: number
  arbiter_timeout_seconds: number
  engine_max_attempts: number
  arbiter_max_attempts: number
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

export type DocumentStatus =
  | "preparing"
  | "ready"
  | "processing"
  | "completed"
  | "failed"
  | "cancelled"

export type DocumentPageStatus =
  | "preparing"
  | "queued"
  | "processing"
  | "completed"
  | "failed"

export interface DocumentPageRecord {
  id: string
  source_page: number
  page_number: number
  status: DocumentPageStatus
  image_ready: boolean
  result_ready: boolean
  confidence?: number
  segments?: number
  pending_disputes?: number
  duration_ms?: number
  revision: number
  error?: string
  updated_at: string
}

export interface DocumentProjectRecord {
  id: string
  user_id: string
  username: string
  name: string
  source_type: "pdf" | "image"
  mime_type: string
  size_bytes: number
  status: DocumentStatus
  page_count: number
  prepared_pages: number
  processed_pages: number
  failed_pages: number
  pending_disputes: number
  engines: string[]
  arbiter?: string
  auto_arbitrate: boolean
  created_at: string
  updated_at: string
  completed_at?: string
  error?: string
  pages?: DocumentPageRecord[]
}

export interface DocumentDisputeItem {
  page_id: string
  page_number: number
  source_page: number
  segment_index: number
  segment: FinalSegment
}

export interface DocumentRunSettings {
  engines: string[]
  arbiter: string
  autoArbitrate: boolean
}

export type DocumentProgressStage =
  | "queued"
  | "loading"
  | "engine"
  | "merge"
  | "arbiter"
  | "saving"
  | "complete"

export interface DocumentAgentProgress {
  agent: string
  stage: "engine" | "arbiter"
  status:
    | "waiting"
    | "thinking"
    | "streaming"
    | "retrying"
    | "completed"
    | "failed"
  started_at: string
  elapsed_ms: number
  first_token: boolean
  ttft_ms?: number
  output_chars: number
  estimated_tokens: number
  tps: number
  thinking?: string
  output?: string
  error?: string
  attempt: number
  max_attempts: number
  last_error?: string
}

export interface DocumentProgressEvent {
  type: "progress"
  sequence: number
  document_id: string
  document_status: DocumentStatus
  page_id?: string
  page_number?: number
  stage: DocumentProgressStage
  status: "running" | DocumentStatus | DocumentPageStatus
  started_at?: string
  elapsed_ms: number
  completed_engines: number
  total_engines: number
  agents: DocumentAgentProgress[]
  error?: string
}

export async function fetchDocuments(): Promise<DocumentProjectRecord[]> {
  const res = await apiFetch("/api/documents")
  await assertOK(res, "获取文档项目")
  return res.json() as Promise<DocumentProjectRecord[]>
}

export async function fetchDocument(
  id: string,
): Promise<DocumentProjectRecord> {
  const res = await apiFetch(`/api/documents/${encodeURIComponent(id)}`)
  await assertOK(res, "获取文档项目")
  return res.json() as Promise<DocumentProjectRecord>
}

export async function streamDocumentProgress(
  id: string,
  onProgress: (progress: DocumentProgressEvent) => void,
  signal?: AbortSignal,
): Promise<void> {
  const res = await apiFetch(
    `/api/documents/${encodeURIComponent(id)}/events`,
    { signal },
  )
  await assertOK(res, "订阅文档识别进度")
  if (!res.body) throw new Error("浏览器未提供流式响应体")

  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ""
  const consume = (line: string) => {
    if (line.trim()) onProgress(JSON.parse(line) as DocumentProgressEvent)
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
}

export function uploadDocument(
  file: File,
  settings: DocumentRunSettings,
  onProgress?: (loaded: number, total: number) => void,
  signal?: AbortSignal,
): Promise<DocumentProjectRecord> {
  if (file.size > 1024 * 1024 * 1024) {
    return Promise.reject(new Error("文档不能超过 1 GiB"))
  }
  const params = new URLSearchParams({
    filename: file.name,
    engines: settings.engines.join(","),
    arbiter: settings.arbiter,
    auto_arbitrate: String(settings.autoArbitrate),
  })
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open("POST", `/api/documents?${params}`)
    xhr.responseType = "json"
    xhr.setRequestHeader(
      "Content-Type",
      file.type || "application/octet-stream",
    )
    if (csrfToken) xhr.setRequestHeader("X-CSRF-Token", csrfToken)
    xhr.upload.onprogress = (event) =>
      onProgress?.(
        event.loaded,
        event.lengthComputable ? event.total : file.size,
      )
    xhr.onerror = () => reject(new Error("上传文档时连接中断"))
    xhr.onabort = () => reject(new DOMException("上传已取消", "AbortError"))
    xhr.onload = () => {
      if (xhr.status === 401) {
        csrfToken = ""
        window.dispatchEvent(new CustomEvent("betterocr:unauthorized"))
      }
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(xhr.response as DocumentProjectRecord)
        return
      }
      const response = xhr.response as { error?: string } | null
      reject(new Error(response?.error || `上传请求失败 (HTTP ${xhr.status})`))
    }
    const abort = () => xhr.abort()
    signal?.addEventListener("abort", abort, { once: true })
    xhr.onloadend = () => signal?.removeEventListener("abort", abort)
    xhr.send(file)
  })
}

export async function runDocument(
  id: string,
  settings: DocumentRunSettings,
): Promise<DocumentProjectRecord> {
  return requestJSON<DocumentProjectRecord>(
    `/api/documents/${encodeURIComponent(id)}/run`,
    "POST",
    {
      engines: settings.engines,
      arbiter: settings.arbiter,
      auto_arbitrate: settings.autoArbitrate,
    },
    "启动文档识别",
  )
}

export async function cancelDocument(
  id: string,
): Promise<DocumentProjectRecord> {
  const res = await apiFetch(
    `/api/documents/${encodeURIComponent(id)}/cancel`,
    {
      method: "POST",
    },
  )
  await assertOK(res, "取消文档任务")
  return res.json() as Promise<DocumentProjectRecord>
}

export async function deleteDocument(id: string): Promise<void> {
  const res = await apiFetch(`/api/documents/${encodeURIComponent(id)}`, {
    method: "DELETE",
  })
  await assertOK(res, "删除文档项目")
}

export async function updateDocumentPageOrder(
  id: string,
  pageIDs: string[],
): Promise<DocumentProjectRecord> {
  return requestJSON<DocumentProjectRecord>(
    `/api/documents/${encodeURIComponent(id)}/pages/order`,
    "PUT",
    { page_ids: pageIDs },
    "调整页序",
  )
}

export async function deleteDocumentPage(
  documentID: string,
  pageID: string,
): Promise<DocumentProjectRecord> {
  const res = await apiFetch(
    `/api/documents/${encodeURIComponent(documentID)}/pages/${encodeURIComponent(pageID)}`,
    { method: "DELETE" },
  )
  await assertOK(res, "删除页面")
  return res.json() as Promise<DocumentProjectRecord>
}

export async function fetchDocumentPageResult(
  documentID: string,
  pageID: string,
): Promise<Final> {
  const res = await apiFetch(
    `/api/documents/${encodeURIComponent(documentID)}/pages/${encodeURIComponent(pageID)}/result`,
  )
  await assertOK(res, "获取页面结果")
  return res.json() as Promise<Final>
}

export async function updateDocumentPageResult(
  documentID: string,
  pageID: string,
  result: Final,
): Promise<DocumentProjectRecord> {
  return requestJSON<DocumentProjectRecord>(
    `/api/documents/${encodeURIComponent(documentID)}/pages/${encodeURIComponent(pageID)}/result`,
    "PUT",
    result,
    "保存审计结果",
  )
}

export async function fetchDocumentDisputes(
  documentID: string,
): Promise<DocumentDisputeItem[]> {
  const res = await apiFetch(
    `/api/documents/${encodeURIComponent(documentID)}/disputes`,
  )
  await assertOK(res, "获取统一审计清单")
  return res.json() as Promise<DocumentDisputeItem[]>
}

export function documentPageImageURL(
  documentID: string,
  pageID: string,
): string {
  return `/api/documents/${encodeURIComponent(documentID)}/pages/${encodeURIComponent(pageID)}/image`
}

export function documentExportURL(
  documentID: string,
  format: "text" | "audit",
): string {
  return `/api/documents/${encodeURIComponent(documentID)}/export/${format}`
}

export interface OCRRequest {
  image: File
  engines: string[]
  arbiter: string
  autoArbitrate: boolean
  signal?: AbortSignal
}

export interface OCRDelta {
  type: "delta" | "attempt_start" | "attempt_failed"
  stage: "engine" | "arbiter"
  agent: string
  kind: "thinking" | "output"
  text: string
  attempt?: number
  max_attempts?: number
  reset?: boolean
  error?: string
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
  type:
    | "start"
    | "delta"
    | "attempt_start"
    | "attempt_failed"
    | "result"
    | "error"
  stage?: OCRDelta["stage"]
  agent?: string
  kind?: OCRDelta["kind"]
  text?: string
  result?: Final
  resolutions?: Resolution[]
  error?: string
  attempt?: number
  max_attempts?: number
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
            type: "delta",
            stage: event.stage,
            agent: event.agent,
            kind: event.kind ?? "output",
            text: event.text,
            attempt: event.attempt,
            max_attempts: event.max_attempts,
          })
        }
        break
      case "attempt_start":
      case "attempt_failed":
        if (event.stage && event.agent) {
          onDelta?.({
            type: event.type,
            stage: event.stage,
            agent: event.agent,
            kind: "output",
            text: "",
            attempt: event.attempt,
            max_attempts: event.max_attempts,
            reset: event.type === "attempt_start",
            error: event.error,
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
