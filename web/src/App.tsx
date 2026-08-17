import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import {
  AlertCircle,
  ArrowRight,
  Bot,
  Check,
  Copy,
  ImagePlus,
  Layers3,
  Loader2,
  Moon,
  ScanText,
  Sun,
  X,
} from "lucide-react"

import {
  fetchConfig,
  runOCR,
  type EngineResult,
  type Final,
  type FinalLine,
  type LineSource,
  type ServerConfig,
} from "@/lib/api"
import { cn } from "@/lib/utils"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { ModelConfigDialog } from "@/components/model-config-dialog"
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Progress } from "@/components/ui/progress"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"

const sourceMeta: Record<LineSource, { label: string; cls: string }> = {
  consensus: {
    label: "共识",
    cls: "border-emerald-500/25 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400",
  },
  escalated: {
    label: "仲裁",
    cls: "border-violet-500/25 bg-violet-500/10 text-violet-700 dark:text-violet-400",
  },
  fallback: {
    label: "兜底",
    cls: "border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-500",
  },
}

const pct = (c: number) => `${(c * 100).toFixed(1)}%`

const confColor = (c: number) =>
  c >= 0.9
    ? "text-emerald-600 dark:text-emerald-400"
    : c >= 0.7
      ? "text-amber-600 dark:text-amber-400"
      : "text-red-600 dark:text-red-400"

const selectionStorageKey = "betterocr-model-selection"

export default function App() {
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
  const abortRef = useRef<AbortController | null>(null)

  // —— 服务端模型目录与本地选择 ——
  const [cfg, setCfg] = useState<ServerConfig | null>(null)
  const [engines, setEngines] = useState<string[]>([])
  const [arbiter, setArbiter] = useState("")

  useEffect(() => {
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

  const modelIndex = useMemo(() => {
    const index = new Map<string, { alias: string; provider: string }>()
    for (const provider of cfg?.providers ?? []) {
      for (const model of provider.models) {
        index.set(`${provider.id}/${model.id}`, {
          alias: model.alias,
          provider: provider.id,
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
    setElapsed(0)
    const started = performance.now()
    const timer = window.setInterval(
      () => setElapsed((performance.now() - started) / 1000),
      100,
    )
    const ac = new AbortController()
    abortRef.current = ac
    try {
      const final = await runOCR({
        image: file,
        engines,
        arbiter,
        signal: ac.signal,
      })
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
              多引擎融合 · 只有分歧行才动用强模型
            </span>
          </div>
          <div className="ms-auto flex items-center gap-1.5">
            <ModelConfigDialog
              config={cfg}
              engines={engines}
              arbiter={arbiter}
              onApply={applyModelSelection}
            />
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
                engines.map((ref, index) => (
                  <Badge
                    key={`${ref}-${index}`}
                    variant="secondary"
                    title={ref}
                  >
                    {modelIndex.get(ref)?.alias ?? ref}
                  </Badge>
                ))
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
                ? (modelIndex.get(arbiter)?.alias ?? arbiter)
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
              <div className="relative w-full">
                <img
                  src={preview}
                  alt="待识别图片"
                  className="mx-auto max-h-[420px] rounded-md object-contain"
                />
                <div className="absolute right-2 top-2 flex gap-2">
                  <Button
                    variant="secondary"
                    size="sm"
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
                    className="size-8"
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
                <p className="mt-2 text-center text-xs text-muted-foreground">
                  {file?.name} · {((file?.size ?? 0) / 1024).toFixed(0)} KB
                </p>
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

        <div className="flex items-center gap-3">
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
        </div>

        {error && (
          <Alert variant="destructive">
            <AlertCircle />
            <AlertTitle>出错了</AlertTitle>
            <AlertDescription className="break-all">{error}</AlertDescription>
          </Alert>
        )}

        {result && <ResultView final={result} />}
      </main>
    </div>
  )
}

function ResultView({ final }: { final: Final }) {
  const s = final.stats
  const json = JSON.stringify(final, null, 2)
  return (
    <Card>
      <CardHeader>
        <CardTitle>识别结果</CardTitle>
        <CardDescription className="flex flex-wrap items-center gap-1.5 pt-1">
          <Badge variant="secondary">{s.engines} 引擎</Badge>
          <Badge variant="outline" className={sourceMeta.consensus.cls}>
            共识 {s.consensus_rows}
          </Badge>
          {s.escalated_rows > 0 && (
            <Badge variant="outline" className={sourceMeta.escalated.cls}>
              仲裁 {s.escalated_rows}
            </Badge>
          )}
          {s.fallback_rows > 0 && (
            <Badge variant="outline" className={sourceMeta.fallback.cls}>
              兜底 {s.fallback_rows}
            </Badge>
          )}
          {(s.dropped_rows ?? 0) > 0 && <Badge variant="outline">丢弃 {s.dropped_rows}</Badge>}
          {s.failed_engines > 0 && <Badge variant="destructive">失败 {s.failed_engines}</Badge>}
          {s.escalator && (
            <Badge variant="outline" className="max-w-48 truncate" title={s.escalator}>
              {s.escalator}
            </Badge>
          )}
        </CardDescription>
        <CardAction className="flex w-32 flex-col items-end gap-1.5">
          <span className="text-2xl font-semibold leading-none tabular-nums">
            {pct(final.confidence)}
          </span>
          <Progress value={final.confidence * 100} />
          <span className="text-xs text-muted-foreground">总置信度</span>
        </CardAction>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {s.escalation_err && (
          <Alert>
            <AlertCircle />
            <AlertTitle>仲裁调用失败,分歧行已本地兜底</AlertTitle>
            <AlertDescription className="break-all">{s.escalation_err}</AlertDescription>
          </Alert>
        )}
        <Tabs defaultValue="text">
          <TabsList>
            <TabsTrigger value="text">文本</TabsTrigger>
            <TabsTrigger value="lines">逐行({final.lines.length})</TabsTrigger>
            <TabsTrigger value="engines">引擎对比</TabsTrigger>
            <TabsTrigger value="json">JSON</TabsTrigger>
          </TabsList>

          <TabsContent value="text">
            <div className="relative">
              <pre className="max-h-[480px] min-h-24 overflow-auto whitespace-pre-wrap rounded-lg border bg-muted/40 p-4 font-sans text-sm leading-7">
                {final.text || "(未识别到文本)"}
              </pre>
              <CopyButton text={final.text} className="absolute right-2 top-2" />
            </div>
          </TabsContent>

          <TabsContent value="lines">
            {final.lines.length === 0 ? (
              <p className="rounded-lg border p-6 text-center text-sm text-muted-foreground">
                没有产出任何行
              </p>
            ) : (
              <div className="overflow-hidden rounded-lg border">
                {final.lines.map((l, i) => (
                  <LineRow key={i} line={l} index={i} />
                ))}
              </div>
            )}
          </TabsContent>

          <TabsContent value="engines">
            <div className="grid gap-3 xl:grid-cols-2">
              {final.candidates.map((c) => (
                <EngineCard key={c.agent} result={c} />
              ))}
            </div>
          </TabsContent>

          <TabsContent value="json">
            <div className="relative">
              <pre className="max-h-[480px] overflow-auto rounded-lg border bg-muted/40 p-4 font-mono text-xs leading-5">
                {json}
              </pre>
              <CopyButton text={json} className="absolute right-2 top-2" />
            </div>
          </TabsContent>
        </Tabs>
      </CardContent>
    </Card>
  )
}

function LineRow({ line, index }: { line: FinalLine; index: number }) {
  const meta = sourceMeta[line.source]
  return (
    <div className="flex items-start gap-3 border-b px-3 py-2 text-sm last:border-b-0 hover:bg-muted/40">
      <span className="mt-0.5 w-6 shrink-0 text-right text-xs tabular-nums text-muted-foreground">
        {index + 1}
      </span>
      <span className="min-w-0 flex-1 whitespace-pre-wrap break-words">{line.text}</span>
      <div className="flex shrink-0 items-center gap-2">
        <span
          className="hidden max-w-44 truncate text-xs text-muted-foreground md:inline"
          title={line.from.join(", ")}
        >
          {line.from.join(" · ")}
        </span>
        <span className={cn("text-xs tabular-nums", confColor(line.confidence))}>
          {pct(line.confidence)}
        </span>
        <Badge variant="outline" className={meta.cls}>
          {meta.label}
        </Badge>
      </div>
    </div>
  )
}

function EngineCard({ result }: { result: EngineResult }) {
  const lines = result.lines ?? []
  return (
    <Card className="gap-3 py-4">
      <CardHeader className="px-4">
        <CardTitle className="truncate text-sm font-medium" title={result.agent}>
          {result.agent}
        </CardTitle>
        <CardAction>
          <Badge variant={result.err ? "destructive" : "secondary"}>
            {result.err ? "失败" : `${(result.latency_ms / 1000).toFixed(1)}s`}
          </Badge>
        </CardAction>
      </CardHeader>
      <CardContent className="px-4">
        {result.err ? (
          <p className="break-all text-xs text-destructive">{result.err}</p>
        ) : lines.length === 0 ? (
          <p className="text-xs text-muted-foreground">(无输出行)</p>
        ) : (
          <ol className="flex max-h-64 flex-col gap-1 overflow-auto text-xs">
            {lines.map((l, i) => (
              <li key={i} className="flex items-baseline gap-2">
                <span className="shrink-0 tabular-nums text-muted-foreground">{i + 1}.</span>
                <span className="min-w-0 flex-1 break-words">{l.text}</span>
                <span className="shrink-0 tabular-nums text-muted-foreground">
                  {pct(l.confidence)}
                </span>
              </li>
            ))}
          </ol>
        )}
      </CardContent>
    </Card>
  )
}

function CopyButton({ text, className }: { text: string; className?: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <Button
      variant="secondary"
      size="icon"
      className={cn("size-8", className)}
      aria-label="复制"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text)
        } catch {
          // 非安全上下文(如局域网 http)下剪贴板不可用,静默忽略
        }
        setCopied(true)
        window.setTimeout(() => setCopied(false), 1500)
      }}
    >
      {copied ? <Check className="text-emerald-500" /> : <Copy />}
    </Button>
  )
}
