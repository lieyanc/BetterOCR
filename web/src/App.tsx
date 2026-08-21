import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import {
  AlertCircle,
  ArrowRight,
  Bot,
  BrainCircuit,
  ImagePlus,
  Layers3,
  Loader2,
  LogOut,
  Moon,
  ScanText,
  Sun,
  X,
} from "lucide-react"

import {
  fetchConfig,
  fetchSession,
  fetchSetupStatus,
  logout,
  runOCR,
  type AuthSession,
  type Final,
  type OCRDelta,
  type ServerConfig,
} from "@/lib/api"
import { cn } from "@/lib/utils"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldLabel } from "@/components/ui/field"
import { ModelConfigDialog } from "@/components/model-config-dialog"
import { ResultWorkbench } from "@/components/result-workbench"
import { AdminDialog } from "@/components/admin-dialog"
import { LoginPage } from "@/components/login-page"
import { SetupPage } from "@/components/setup-page"
import { TaskHistoryDialog } from "@/components/task-history-dialog"
import {
  Card,
  CardAction,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"

const selectionStorageKey = "betterocr-model-selection"

interface LiveOutput {
  stage: OCRDelta["stage"]
  agent: string
  thinking: string
  answer: string
}

export default function App() {
  const [session, setSession] = useState<AuthSession | null>(null)
  const [checkingSession, setCheckingSession] = useState(true)
  const [setupRequired, setSetupRequired] = useState(false)
  const [startupError, setStartupError] = useState("")

  useEffect(() => {
    const unauthorized = () => setSession(null)
    window.addEventListener("betterocr:unauthorized", unauthorized)
    const start = async () => {
      try {
        const status = await fetchSetupStatus()
        setSetupRequired(!status.initialized)
        if (status.initialized) {
          try {
            setSession(await fetchSession())
          } catch {
            setSession(null)
          }
        }
      } catch (cause) {
        setStartupError(
          cause instanceof Error ? cause.message : "连接 BetterOCR 失败",
        )
      } finally {
        setCheckingSession(false)
      }
    }
    void start()
    return () =>
      window.removeEventListener("betterocr:unauthorized", unauthorized)
  }, [])

  if (checkingSession) {
    return (
      <div className="flex min-h-screen items-center justify-center text-sm text-muted-foreground">
        <Loader2 className="me-2 size-4 animate-spin" />
        正在连接 BetterOCR
      </div>
    )
  }

  if (startupError) {
    return (
      <div className="flex min-h-screen items-center justify-center p-4">
        <Alert variant="destructive" className="max-w-md">
          <AlertCircle />
          <AlertTitle>无法启动应用</AlertTitle>
          <AlertDescription>{startupError}</AlertDescription>
        </Alert>
      </div>
    )
  }

  if (setupRequired) {
    return (
      <SetupPage
        onInitialized={(nextSession) => {
          setSetupRequired(false)
          setSession(nextSession)
        }}
      />
    )
  }

  if (!session) return <LoginPage onLogin={setSession} />

  return (
    <OCRWorkspace
      session={session}
      onLogout={async () => {
        try {
          await logout()
        } finally {
          setSession(null)
        }
      }}
    />
  )
}

function OCRWorkspace({
  session,
  onLogout,
}: {
  session: AuthSession
  onLogout: () => Promise<void>
}) {
  // —— 主题 ——
  const [dark, setDark] = useState(() =>
    document.documentElement.classList.contains("dark"),
  )
  const toggleTheme = () => {
    const next = !dark
    setDark(next)
    document.documentElement.classList.toggle("dark", next)
    localStorage.setItem("betterocr-theme", next ? "dark" : "light")
  }

  // —— 识别状态 ——
  const [busy, setBusy] = useState(false)
  const [elapsed, setElapsed] = useState(0)
  const [error, setError] = useState("")
  const [result, setResult] = useState<Final | null>(null)
  const [liveOutputs, setLiveOutputs] = useState<LiveOutput[]>([])
  const abortRef = useRef<AbortController | null>(null)

  // —— 服务端模型目录与本地选择 ——
  const [cfg, setCfg] = useState<ServerConfig | null>(null)
  const [engines, setEngines] = useState<string[]>([])
  const [arbiter, setArbiter] = useState("")
  const [autoArbitrate, setAutoArbitrate] = useState(true)

  const loadConfig = useCallback(() => {
    fetchConfig()
      .then((c) => {
        const validRefs = new Set(
          c.providers.flatMap((provider) =>
            provider.models.map((model) => `${provider.id}/${model.id}`),
          ),
        )
        let nextEngines = c.engines
        let nextArbiter = c.arbiter
        try {
          const saved = JSON.parse(
            localStorage.getItem(selectionStorageKey) ?? "null",
          ) as {
            engines?: unknown
            arbiter?: unknown
          } | null
          if (Array.isArray(saved?.engines)) {
            const filtered = saved.engines.filter(
              (ref): ref is string =>
                typeof ref === "string" && validRefs.has(ref),
            )
            if (filtered.length > 0) nextEngines = filtered
          }
          if (
            typeof saved?.arbiter === "string" &&
            (saved.arbiter === "" || validRefs.has(saved.arbiter))
          ) {
            nextArbiter = saved.arbiter
          }
        } catch {
          localStorage.removeItem(selectionStorageKey)
        }
        setCfg(c)
        setEngines(nextEngines)
        setArbiter(nextArbiter)
      })
      .catch((cause) => {
        setError(cause instanceof Error ? cause.message : "加载模型配置失败")
      })
  }, [])

  useEffect(() => {
    void loadConfig()
  }, [loadConfig])

  useEffect(() => () => abortRef.current?.abort(), [])

  const modelIndex = useMemo(() => {
    const index = new Map<string, { alias: string; provider: string }>()
    for (const provider of cfg?.providers ?? []) {
      for (const model of provider.models) {
        index.set(`${provider.id}/${model.id}`, {
          alias: model.alias,
          provider: provider.alias,
        })
      }
    }
    return index
  }, [cfg])

  const applyModelSelection = (nextEngines: string[], nextArbiter: string) => {
    setEngines(nextEngines)
    setArbiter(nextArbiter)
    localStorage.setItem(
      selectionStorageKey,
      JSON.stringify({ engines: nextEngines, arbiter: nextArbiter }),
    )
  }

  // —— 图片选择:点击 / 拖拽 / 粘贴 ——
  const [file, setFile] = useState<File | null>(null)
  const [preview, setPreview] = useState("")
  const [dragging, setDragging] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)

  const acceptFile = useCallback((f: File | null | undefined) => {
    if (!f || !f.type.startsWith("image/")) return
    setFile(f)
    setResult(null)
    setError("")
  }, [])

  useEffect(() => {
    if (!file) {
      setPreview("")
      return
    }
    const url = URL.createObjectURL(file)
    setPreview(url)
    return () => URL.revokeObjectURL(url)
  }, [file])

  useEffect(() => {
    const onPaste = (e: ClipboardEvent) => {
      const item = Array.from(e.clipboardData?.items ?? []).find((i) =>
        i.type.startsWith("image/"),
      )
      if (item) acceptFile(item.getAsFile())
    }
    window.addEventListener("paste", onPaste)
    return () => window.removeEventListener("paste", onPaste)
  }, [acceptFile])

  const run = async () => {
    if (!file) return
    if (engines.length === 0) {
      setError("请至少选择一个基础模型")
      return
    }
    setBusy(true)
    setError("")
    setResult(null)
    setLiveOutputs([])
    setElapsed(0)
    const started = performance.now()
    const timer = window.setInterval(
      () => setElapsed((performance.now() - started) / 1000),
      100,
    )
    const ac = new AbortController()
    abortRef.current = ac
    try {
      const final = await runOCR(
        {
          image: file,
          engines,
          arbiter,
          autoArbitrate,
          signal: ac.signal,
        },
        (delta) => {
          setLiveOutputs((current) => {
            const index = current.findIndex(
              (item) =>
                item.stage === delta.stage && item.agent === delta.agent,
            )
            if (index < 0) {
              return [
                ...current,
                {
                  stage: delta.stage,
                  agent: delta.agent,
                  thinking: delta.kind === "thinking" ? delta.text : "",
                  answer: delta.kind === "output" ? delta.text : "",
                },
              ]
            }
            return current.map((item, i) =>
              i === index
                ? delta.kind === "thinking"
                  ? { ...item, thinking: item.thinking + delta.text }
                  : { ...item, answer: item.answer + delta.text }
                : item,
            )
          })
        },
      )
      setResult(final)
      if (
        final.stats.engines > 0 &&
        final.stats.failed_engines === final.stats.engines
      ) {
        setError(
          "所有引擎均失败,请检查 Provider 连接与模型配置,细节见「引擎对比」",
        )
      }
    } catch (e) {
      if (e instanceof DOMException && e.name === "AbortError") {
        setError("已取消")
      } else {
        setError(e instanceof Error ? e.message : String(e))
      }
    } finally {
      window.clearInterval(timer)
      setBusy(false)
      abortRef.current = null
    }
  }

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-10 border-b bg-background/80 backdrop-blur">
        <div className="mx-auto flex h-14 max-w-5xl items-center gap-3 px-4 md:px-6">
          <div className="flex size-8 items-center justify-center rounded-lg bg-primary text-primary-foreground">
            <ScanText className="size-4.5" />
          </div>
          <div className="min-w-0 flex-1 sm:flex-none">
            <span className="text-sm font-semibold leading-none">
              BetterOCR
            </span>
            <span className="hidden text-xs text-muted-foreground sm:block">
              全文识别 · 中文句段动态融合
            </span>
          </div>
          <div className="ms-auto flex items-center gap-1.5">
            <TaskHistoryDialog user={session.user} />
            {session.user.role === "admin" && (
              <AdminDialog
                currentUser={session.user}
                onSettingsChanged={() => void loadConfig()}
              />
            )}
            <ModelConfigDialog
              config={cfg}
              engines={engines}
              arbiter={arbiter}
              onApply={applyModelSelection}
            />
            <Badge variant="outline" className="hidden md:inline-flex">
              {session.user.username}
            </Badge>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon"
                  onClick={toggleTheme}
                  aria-label="切换明暗主题"
                >
                  {dark ? <Sun /> : <Moon />}
                </Button>
              </TooltipTrigger>
              <TooltipContent>
                {dark ? "切换到浅色" : "切换到深色"}
              </TooltipContent>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon"
                  onClick={() => void onLogout()}
                  aria-label="退出登录"
                >
                  <LogOut />
                </Button>
              </TooltipTrigger>
              <TooltipContent>退出登录</TooltipContent>
            </Tooltip>
          </div>
        </div>
      </header>

      <main className="mx-auto flex max-w-5xl flex-col gap-4 p-4 md:p-6">
        <div className="flex flex-wrap items-center gap-3 py-1">
          <div className="flex min-w-0 basis-full items-center gap-2.5 sm:flex-1 sm:basis-auto">
            <Layers3 className="size-4 shrink-0 text-muted-foreground" />
            <div className="flex min-w-0 flex-wrap items-center gap-1.5">
              <span className="me-1 text-xs font-medium text-muted-foreground">
                基础模型
              </span>
              {engines.length > 0 ? (
                engines.map((ref, index) => {
                  const m = modelIndex.get(ref)
                  return (
                    <Badge
                      key={`${ref}-${index}`}
                      variant="secondary"
                      title={ref}
                    >
                      {m ? `${m.provider} · ${m.alias}` : ref}
                    </Badge>
                  )
                })
              ) : (
                <span className="text-sm text-muted-foreground">
                  {cfg ? "未选择" : "加载中"}
                </span>
              )}
            </div>
          </div>
          <ArrowRight className="hidden size-4 shrink-0 text-muted-foreground sm:block" />
          <div className="flex min-w-0 basis-full items-center gap-2 sm:basis-auto">
            <Bot className="size-4 shrink-0 text-muted-foreground" />
            <span className="text-xs font-medium text-muted-foreground">
              仲裁
            </span>
            <Badge variant="outline" title={arbiter || undefined}>
              {arbiter
                ? (() => {
                    const m = modelIndex.get(arbiter)
                    return m ? `${m.provider} · ${m.alias}` : arbiter
                  })()
                : "本地兜底"}
            </Badge>
          </div>
        </div>

        <Card
          className={cn(
            "border-dashed py-4 transition-colors",
            dragging && "border-primary bg-primary/5",
            !preview && "cursor-pointer hover:border-primary/50",
          )}
          onDragOver={(e) => {
            e.preventDefault()
            setDragging(true)
          }}
          onDragLeave={() => setDragging(false)}
          onDrop={(e) => {
            e.preventDefault()
            setDragging(false)
            acceptFile(e.dataTransfer.files?.[0])
          }}
          onClick={() => {
            if (!preview) fileInput.current?.click()
          }}
        >
          <CardContent className="flex min-h-48 items-center justify-center px-4">
            {preview ? (
              <div className="flex min-w-0 w-full flex-col gap-3">
                <img
                  src={preview}
                  alt="待识别图片"
                  className="mx-auto max-h-[420px] rounded-md object-contain"
                />
                <div className="flex min-w-0 items-center gap-2">
                  <p
                    className="min-w-0 flex-1 truncate text-center text-xs text-muted-foreground"
                    title={file?.name}
                  >
                    {file?.name} · {((file?.size ?? 0) / 1024).toFixed(0)} KB
                  </p>
                  <Button
                    variant="secondary"
                    size="sm"
                    className="shrink-0"
                    onClick={(e) => {
                      e.stopPropagation()
                      fileInput.current?.click()
                    }}
                  >
                    <ImagePlus data-icon="inline-start" />
                    更换
                  </Button>
                  <Button
                    variant="secondary"
                    size="icon"
                    className="size-8 shrink-0"
                    aria-label="清除图片"
                    onClick={(e) => {
                      e.stopPropagation()
                      setFile(null)
                      setResult(null)
                    }}
                  >
                    <X />
                  </Button>
                </div>
              </div>
            ) : (
              <div className="flex flex-col items-center gap-2 py-8 text-muted-foreground">
                <ImagePlus className="size-10" />
                <p className="text-sm">拖拽、点击或 Ctrl+V 粘贴图片</p>
                <p className="text-xs">PNG / JPEG / WebP / GIF</p>
              </div>
            )}
          </CardContent>
        </Card>
        <input
          ref={fileInput}
          type="file"
          accept="image/*"
          className="hidden"
          onChange={(e) => {
            acceptFile(e.target.files?.[0])
            e.target.value = ""
          }}
        />

        <div className="flex flex-wrap items-center gap-3">
          <Button
            size="lg"
            className="min-w-36"
            disabled={busy || !file}
            onClick={run}
          >
            {busy ? (
              <>
                <Loader2 data-icon="inline-start" className="animate-spin" />
                识别中 {elapsed.toFixed(1)}s
              </>
            ) : (
              <>
                <ScanText data-icon="inline-start" />
                开始识别
              </>
            )}
          </Button>
          {busy && (
            <Button
              variant="outline"
              size="lg"
              onClick={() => abortRef.current?.abort()}
            >
              取消
            </Button>
          )}
          {result && !busy && (
            <span className="text-sm text-muted-foreground">
              耗时 {elapsed.toFixed(1)}s
            </span>
          )}
          <Field orientation="horizontal" className="ms-auto w-auto">
            <Checkbox
              id="auto-arbitrate"
              checked={Boolean(arbiter) && autoArbitrate}
              disabled={!arbiter || busy}
              onCheckedChange={(checked) => setAutoArbitrate(checked === true)}
            />
            <FieldLabel htmlFor="auto-arbitrate" className="whitespace-nowrap">
              自动仲裁
            </FieldLabel>
          </Field>
        </div>

        {error && (
          <Alert variant="destructive">
            <AlertCircle />
            <AlertTitle>出错了</AlertTitle>
            <AlertDescription className="break-all">{error}</AlertDescription>
          </Alert>
        )}

        {busy && <LiveOutputView outputs={liveOutputs} />}
        {result && file && (
          <ResultWorkbench final={result} image={file} arbiter={arbiter} />
        )}
      </main>
    </div>
  )
}

function LiveOutputView({ outputs }: { outputs: LiveOutput[] }) {
  return (
    <section className="flex flex-col gap-3" aria-label="模型实时输出">
      <div className="flex items-center gap-2">
        <Loader2 className="size-4 animate-spin text-muted-foreground" />
        <h2 className="text-sm font-semibold">实时输出</h2>
        <Badge variant="secondary" className="tabular-nums">
          {outputs.length} 路
        </Badge>
      </div>
      {outputs.length === 0 ? (
        <div className="flex min-h-28 items-center justify-center rounded-md border border-dashed text-sm text-muted-foreground">
          等待模型响应
        </div>
      ) : (
        <div className="grid gap-3 xl:grid-cols-2">
          {outputs.map((output) => (
            <LiveOutputCard
              key={`${output.stage}\u0000${output.agent}`}
              output={output}
            />
          ))}
        </div>
      )}
    </section>
  )
}

function LiveOutputCard({ output }: { output: LiveOutput }) {
  const thinkingRef = useRef<HTMLPreElement>(null)
  const answerRef = useRef<HTMLPreElement>(null)
  useEffect(() => {
    const thinking = thinkingRef.current
    const answer = answerRef.current
    if (thinking) thinking.scrollTop = thinking.scrollHeight
    if (answer) answer.scrollTop = answer.scrollHeight
  }, [output.thinking, output.answer])

  return (
    <Card className="gap-3 py-4">
      <CardHeader className="px-4">
        <CardTitle
          className="truncate text-sm font-medium"
          title={output.agent}
        >
          {output.agent}
        </CardTitle>
        <CardAction className="flex items-center gap-1.5">
          {output.thinking && (
            <Badge variant="outline">
              <BrainCircuit data-icon="inline-start" />
              思考
            </Badge>
          )}
          <Badge variant={output.stage === "arbiter" ? "default" : "secondary"}>
            {output.stage === "arbiter" ? "仲裁" : "基础模型"}
          </Badge>
        </CardAction>
      </CardHeader>
      <CardContent className="grid gap-3 px-4">
        <section className="grid gap-1.5">
          <div className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
            <BrainCircuit className="size-3.5" />
            思考过程
          </div>
          <pre
            ref={thinkingRef}
            className="h-24 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/50 p-3 font-sans text-xs leading-5 text-muted-foreground"
          >
            {output.thinking || "未提供思考过程"}
          </pre>
        </section>
        <section className="grid gap-1.5">
          <div className="text-xs font-medium text-muted-foreground">
            主输出
          </div>
          <pre
            ref={answerRef}
            className="h-32 overflow-auto whitespace-pre-wrap break-words rounded-md border p-3 font-sans text-sm leading-6"
          >
            {output.answer || "等待主输出"}
          </pre>
        </section>
      </CardContent>
    </Card>
  )
}
