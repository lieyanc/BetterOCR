import { useEffect, useMemo, useRef, useState } from "react"
import {
  AlertCircle,
  Bot,
  BrainCircuit,
  Check,
  CircleCheck,
  Copy,
  Gavel,
  Loader2,
  Pencil,
  RotateCcw,
  UserRoundCheck,
} from "lucide-react"

import {
  runArbitration,
  type Candidate,
  type Dispute,
  type EngineResult,
  type Final,
  type FinalSegment,
  type SegmentSource,
} from "@/lib/api"
import { cn } from "@/lib/utils"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Field, FieldLabel } from "@/components/ui/field"
import { Progress } from "@/components/ui/progress"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"

const sourceMeta: Record<
  SegmentSource,
  { label: string; variant: "default" | "secondary" | "outline" }
> = {
  consensus: { label: "共识", variant: "secondary" },
  escalated: { label: "已仲裁", variant: "default" },
  fallback: { label: "待处理", variant: "outline" },
  user: { label: "人工合并", variant: "secondary" },
}

const pct = (confidence: number) => `${(confidence * 100).toFixed(1)}%`

interface ResultWorkbenchProps {
  final: Final
  image: File
  arbiter: string
  initialTab?: "text" | "disputes" | "segments" | "engines" | "json"
  onChange?: (final: Final) => void
}

export function ResultWorkbench({
  final,
  image,
  arbiter,
  initialTab,
  onChange,
}: ResultWorkbenchProps) {
  const [segments, setSegments] = useState(final.segments)
  const [editing, setEditing] = useState<number | null>(null)
  const [draft, setDraft] = useState("")
  const [arbitrating, setArbitrating] = useState<number[]>([])
  const [arbiterThinking, setArbiterThinking] = useState("")
  const [arbiterOutput, setArbiterOutput] = useState("")
  const [feedback, setFeedback] = useState("")
  const [arbitrationError, setArbitrationError] = useState("")
  const [arbiterAttempt, setArbiterAttempt] = useState({
    attempt: 0,
    max: 0,
    lastError: "",
  })
  const arbitrationAbort = useRef<AbortController | null>(null)
  const arbiterThinkingRef = useRef<HTMLPreElement>(null)
  const arbiterOutputRef = useRef<HTMLPreElement>(null)

  useEffect(() => () => arbitrationAbort.current?.abort(), [])

  useEffect(() => {
    const thinking = arbiterThinkingRef.current
    const output = arbiterOutputRef.current
    if (thinking) thinking.scrollTop = thinking.scrollHeight
    if (output) output.scrollTop = output.scrollHeight
  }, [arbiterThinking, arbiterOutput])

  const disputedIndexes = useMemo(
    () =>
      segments.flatMap((segment, index) => (segment.disputed ? [index] : [])),
    [segments],
  )
  const pendingIndexes = useMemo(
    () =>
      disputedIndexes.filter((index) => segments[index].source === "fallback"),
    [disputedIndexes, segments],
  )
  const mergedText = useMemo(
    () =>
      segments
        .map((segment) => segment.text.trim())
        .filter(Boolean)
        .join("\n"),
    [segments],
  )
  const confidence = useMemo(() => {
    const visible = segments.filter((segment) => segment.text.trim())
    if (visible.length === 0) return 0
    return (
      visible.reduce((sum, segment) => sum + segment.confidence, 0) /
      visible.length
    )
  }, [segments])
  const effectiveFinal = useMemo(
    () => ({ ...final, text: mergedText, confidence, segments }),
    [confidence, final, mergedText, segments],
  )
  const json = JSON.stringify(effectiveFinal, null, 2)
  const sourceCounts = useMemo(
    () =>
      segments.reduce<Record<SegmentSource, number>>(
        (counts, segment) => {
          counts[segment.source]++
          return counts
        },
        { consensus: 0, escalated: 0, fallback: 0, user: 0 },
      ),
    [segments],
  )

  const applySegments = (next: FinalSegment[]) => {
    setSegments(next)
    const visible = next.filter((segment) => segment.text.trim())
    const nextConfidence =
      visible.length === 0
        ? 0
        : visible.reduce((sum, segment) => sum + segment.confidence, 0) /
          visible.length
    onChange?.({
      ...final,
      text: next
        .map((segment) => segment.text.trim())
        .filter(Boolean)
        .join("\n"),
      confidence: nextConfidence,
      segments: next,
    })
  }

  const updateSegment = (index: number, update: Partial<FinalSegment>) => {
    applySegments(
      segments.map((segment, currentIndex) =>
        currentIndex === index ? { ...segment, ...update } : segment,
      ),
    )
  }

  const chooseCandidate = (index: number, candidate: Candidate) => {
    updateSegment(index, {
      text: candidate.text,
      source: "user",
      from: [candidate.agent],
    })
    setEditing(null)
    setFeedback(`句段 ${index + 1} 已采用 ${candidate.agent} 的候选`)
    setArbitrationError("")
    setArbiterAttempt({ attempt: 0, max: 0, lastError: "" })
  }

  const applyDraft = (index: number) => {
    updateSegment(index, {
      text: draft.trim(),
      source: "user",
      from: ["用户编辑"],
    })
    setEditing(null)
    setFeedback(`句段 ${index + 1} 已采用人工编辑内容`)
    setArbitrationError("")
  }

  const arbitrate = async (indexes: number[]) => {
    if (!arbiter) {
      setArbitrationError("请先在模型配置中选择仲裁模型")
      return
    }
    const targets = indexes.filter((index) => segments[index]?.disputed)
    if (targets.length === 0) {
      setFeedback("没有需要仲裁的句段")
      return
    }
    const disputes = targets.map((index) => buildDispute(segments, index))
    const controller = new AbortController()
    arbitrationAbort.current = controller
    setArbitrating(targets)
    setArbiterThinking("")
    setArbiterOutput("")
    setFeedback("")
    setArbitrationError("")
    try {
      const resolutions = await runArbitration(
        { image, arbiter, disputes, signal: controller.signal },
        (delta) => {
          if (delta.type === "attempt_start") {
            setArbiterThinking("")
            setArbiterOutput("")
            setArbiterAttempt((current) => ({
              attempt: delta.attempt ?? current.attempt,
              max: delta.max_attempts ?? current.max,
              lastError: current.lastError,
            }))
            return
          }
          if (delta.type === "attempt_failed") {
            setArbiterAttempt((current) => ({
              ...current,
              lastError: delta.error ?? "",
            }))
            return
          }
          if (delta.kind === "thinking") {
            setArbiterThinking((current) => current + delta.text)
          } else {
            setArbiterOutput((current) => current + delta.text)
          }
        },
      )
      const byIndex = new Map(
        resolutions.map((resolution) => [resolution.segment, resolution]),
      )
      applySegments(
        segments.map((segment, index) => {
          const resolution = byIndex.get(index)
          if (!resolution) return segment
          return {
            ...segment,
            text: resolution.text,
            confidence: resolution.confidence,
            source: "escalated",
            from: resolution.from ?? ["仲裁模型"],
          }
        }),
      )
      const missing = targets.length - resolutions.length
      if (resolutions.length > 0) {
        setArbiterOutput(
          resolutions
            .map(
              (resolution) =>
                `句段 ${resolution.segment + 1}：${resolution.text}`,
            )
            .join("\n"),
        )
      }
      setFeedback(
        missing > 0
          ? `仲裁完成 ${resolutions.length} 个句段，${missing} 个未返回，仍保留原候选`
          : `仲裁完成，已更新 ${resolutions.length} 个句段`,
      )
    } catch (cause) {
      if (cause instanceof DOMException && cause.name === "AbortError") {
        setArbitrationError("仲裁已取消，原合并结果未改变")
      } else {
        setArbitrationError(
          cause instanceof Error ? cause.message : String(cause),
        )
      }
    } finally {
      setArbitrating([])
      arbitrationAbort.current = null
    }
  }

  const stats = final.stats
  return (
    <Card>
      <CardHeader>
        <CardTitle>识别与合并结果</CardTitle>
        <CardDescription className="flex flex-wrap items-center gap-1.5 pt-1">
          <Badge variant="secondary">{stats.engines} 引擎</Badge>
          <Badge variant="secondary">共识 {sourceCounts.consensus}</Badge>
          {sourceCounts.escalated > 0 && (
            <Badge>已仲裁 {sourceCounts.escalated}</Badge>
          )}
          {sourceCounts.user > 0 && (
            <Badge variant="secondary">人工合并 {sourceCounts.user}</Badge>
          )}
          {pendingIndexes.length > 0 && (
            <Badge variant="outline">待处理 {pendingIndexes.length}</Badge>
          )}
          {stats.failed_engines > 0 && (
            <Badge variant="destructive">失败 {stats.failed_engines}</Badge>
          )}
        </CardDescription>
        <CardAction className="flex flex-col items-end gap-2.5">
          <div className="flex w-32 flex-col items-end gap-1.5">
            <span className="text-2xl font-semibold leading-none tabular-nums">
              {pct(confidence)}
            </span>
            <Progress value={confidence * 100} />
            <span className="text-xs text-muted-foreground">当前置信度</span>
          </div>
          <CopyButton text={mergedText} labeled />
        </CardAction>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {stats.escalation_err && (
          <Alert>
            <AlertCircle />
            <AlertTitle>自动仲裁失败，争议已保留</AlertTitle>
            <AlertDescription className="break-all">
              {stats.escalation_err}
            </AlertDescription>
          </Alert>
        )}
        {feedback && (
          <Alert>
            <CircleCheck />
            <AlertTitle>结果已更新</AlertTitle>
            <AlertDescription>{feedback}</AlertDescription>
          </Alert>
        )}
        {arbitrationError && (
          <Alert variant="destructive">
            <AlertCircle />
            <AlertTitle>仲裁未完成</AlertTitle>
            <AlertDescription>{arbitrationError}</AlertDescription>
          </Alert>
        )}
        {(arbitrating.length > 0 || arbiterThinking || arbiterOutput) && (
          <Alert>
            {arbitrating.length > 0 ? (
              <Loader2 className="animate-spin" />
            ) : (
              <Bot />
            )}
            <AlertTitle>
              {arbitrating.length > 0
                ? `正在仲裁 ${arbitrating.length} 个句段${arbiterAttempt.max > 1 ? ` · 尝试 ${arbiterAttempt.attempt || 1}/${arbiterAttempt.max}` : ""}`
                : "最近一次仲裁输出"}
            </AlertTitle>
            <AlertDescription className="grid w-full gap-3">
              {arbitrating.length > 0 && arbiterAttempt.lastError && (
                <p className="break-all text-xs text-muted-foreground">
                  上次尝试失败：{arbiterAttempt.lastError}
                </p>
              )}
              <section className="grid gap-1.5">
                <div className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
                  <BrainCircuit className="size-3.5" />
                  思考过程
                </div>
                <pre
                  ref={arbiterThinkingRef}
                  className="h-24 w-full overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/50 p-3 font-sans text-xs leading-5 text-muted-foreground"
                >
                  {arbiterThinking || "未提供思考过程"}
                </pre>
              </section>
              <section className="grid gap-1.5">
                <div className="text-xs font-medium text-muted-foreground">
                  主输出
                </div>
                <pre
                  ref={arbiterOutputRef}
                  className="h-28 w-full overflow-auto whitespace-pre-wrap break-words rounded-md border p-3 font-sans text-xs leading-5"
                >
                  {arbiterOutput || "等待主输出"}
                </pre>
              </section>
              {arbitrating.length > 0 && (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => arbitrationAbort.current?.abort()}
                >
                  取消仲裁
                </Button>
              )}
            </AlertDescription>
          </Alert>
        )}

        <Tabs
          defaultValue={
            initialTab ?? (pendingIndexes.length > 0 ? "disputes" : "text")
          }
        >
          <TabsList className="grid h-auto w-full grid-cols-5">
            <TabsTrigger value="text">
              <span className="sm:hidden">合并</span>
              <span className="hidden sm:inline">合并文本</span>
            </TabsTrigger>
            <TabsTrigger value="disputes">
              争议({disputedIndexes.length})
            </TabsTrigger>
            <TabsTrigger value="segments">
              <span className="sm:hidden">全部({segments.length})</span>
              <span className="hidden sm:inline">
                全部句段({segments.length})
              </span>
            </TabsTrigger>
            <TabsTrigger value="engines">
              <span className="sm:hidden">原文</span>
              <span className="hidden sm:inline">模型原文</span>
            </TabsTrigger>
            <TabsTrigger value="json">JSON</TabsTrigger>
          </TabsList>

          <TabsContent value="text">
            <div className="relative">
              <pre className="max-h-[520px] min-h-28 overflow-auto whitespace-pre-wrap rounded-md border bg-muted/40 p-4 pr-12 font-sans text-sm leading-7">
                {mergedText || "(未识别到文本)"}
              </pre>
              <CopyButton
                text={mergedText}
                className="absolute right-2 top-2"
              />
            </div>
          </TabsContent>

          <TabsContent value="disputes">
            <div className="flex flex-col gap-3">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <p className="text-sm text-muted-foreground">
                  {pendingIndexes.length > 0
                    ? `${pendingIndexes.length} 个句段等待合并或仲裁`
                    : disputedIndexes.length > 0
                      ? "所有争议句段均已有裁定，可继续调整"
                      : "所有模型文本一致"}
                </p>
                {pendingIndexes.length > 0 && (
                  <Button
                    size="sm"
                    disabled={!arbiter || arbitrating.length > 0}
                    onClick={() => void arbitrate(pendingIndexes)}
                  >
                    {arbitrating.length > 0 ? (
                      <Loader2
                        data-icon="inline-start"
                        className="animate-spin"
                      />
                    ) : (
                      <Gavel data-icon="inline-start" />
                    )}
                    仲裁全部待处理
                  </Button>
                )}
              </div>
              {!arbiter && pendingIndexes.length > 0 && (
                <Alert>
                  <AlertCircle />
                  <AlertTitle>未选择仲裁模型</AlertTitle>
                  <AlertDescription>
                    可以直接选择候选或自定义编辑。
                  </AlertDescription>
                </Alert>
              )}
              {disputedIndexes.length === 0 ? (
                <div className="flex min-h-28 items-center justify-center rounded-md border border-dashed text-sm text-muted-foreground">
                  无争议句段
                </div>
              ) : (
                <div className="overflow-hidden rounded-md border">
                  {disputedIndexes.map((index) => (
                    <DisputeEditor
                      key={index}
                      index={index}
                      segment={segments[index]}
                      editing={editing === index}
                      draft={editing === index ? draft : segments[index].text}
                      busy={arbitrating.includes(index)}
                      arbitrationDisabled={!arbiter || arbitrating.length > 0}
                      onCandidate={(candidate) =>
                        chooseCandidate(index, candidate)
                      }
                      onEdit={() => {
                        setEditing(index)
                        setDraft(segments[index].text)
                      }}
                      onDraftChange={setDraft}
                      onApplyDraft={() => applyDraft(index)}
                      onCancelEdit={() => setEditing(null)}
                      onArbitrate={() => void arbitrate([index])}
                    />
                  ))}
                </div>
              )}
            </div>
          </TabsContent>

          <TabsContent value="segments">
            <div className="overflow-hidden rounded-md border">
              {segments.map((segment, index) => (
                <SegmentRow key={index} segment={segment} index={index} />
              ))}
            </div>
          </TabsContent>

          <TabsContent value="engines">
            <div className="grid gap-3 xl:grid-cols-2">
              {final.candidates.map((candidate) => (
                <EngineOutput key={candidate.agent} result={candidate} />
              ))}
            </div>
          </TabsContent>

          <TabsContent value="json">
            <div className="relative">
              <pre className="max-h-[520px] overflow-auto rounded-md border bg-muted/40 p-4 pr-12 font-mono text-xs leading-5">
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

interface DisputeEditorProps {
  index: number
  segment: FinalSegment
  editing: boolean
  draft: string
  busy: boolean
  arbitrationDisabled: boolean
  onCandidate: (candidate: Candidate) => void
  onEdit: () => void
  onDraftChange: (text: string) => void
  onApplyDraft: () => void
  onCancelEdit: () => void
  onArbitrate: () => void
}

function DisputeEditor({
  index,
  segment,
  editing,
  draft,
  busy,
  arbitrationDisabled,
  onCandidate,
  onEdit,
  onDraftChange,
  onApplyDraft,
  onCancelEdit,
  onArbitrate,
}: DisputeEditorProps) {
  const candidates = segment.candidates ?? []
  const selected = candidates.findIndex(
    (candidate) =>
      segment.source === "user" &&
      segment.from.length === 1 &&
      segment.from[0] === candidate.agent &&
      segment.text === candidate.text,
  )
  const meta = sourceMeta[segment.source]
  return (
    <section
      className="flex flex-col gap-3 border-b p-3 last:border-b-0"
      aria-label={`争议句段 ${index + 1}`}
    >
      <div className="flex flex-wrap items-start gap-2">
        <span className="w-7 shrink-0 text-right text-xs tabular-nums text-muted-foreground">
          {index + 1}
        </span>
        <p className="min-w-0 flex-1 whitespace-pre-wrap break-words text-sm leading-6">
          {segment.text || "(当前判定：图中不存在此句段)"}
        </p>
        <Badge variant={meta.variant}>{meta.label}</Badge>
      </div>

      <ToggleGroup
        type="single"
        variant="outline"
        spacing={2}
        value={selected >= 0 ? String(selected) : ""}
        onValueChange={(value) => {
          const candidate = candidates[Number(value)]
          if (value !== "" && candidate) onCandidate(candidate)
        }}
        className="w-full flex-col items-stretch"
      >
        {candidates.map((candidate, candidateIndex) => (
          <ToggleGroupItem
            key={`${candidate.agent}-${candidateIndex}`}
            value={String(candidateIndex)}
            className="h-auto min-h-12 w-full shrink whitespace-normal px-3 py-2 text-left"
          >
            <span className="flex min-w-0 flex-1 flex-col items-start gap-1">
              <span className="max-w-full truncate text-xs text-muted-foreground">
                {candidate.agent}
              </span>
              <span className="break-words leading-5">{candidate.text}</span>
            </span>
          </ToggleGroupItem>
        ))}
      </ToggleGroup>

      {editing && (
        <Field>
          <FieldLabel htmlFor={`segment-${index}-editor`}>合并文本</FieldLabel>
          <Textarea
            id={`segment-${index}-editor`}
            value={draft}
            onChange={(event) => onDraftChange(event.target.value)}
            className="min-h-24 resize-y"
            autoFocus
          />
          <div className="flex flex-wrap gap-2">
            <Button size="sm" onClick={onApplyDraft}>
              <UserRoundCheck data-icon="inline-start" />
              采用编辑内容
            </Button>
            <Button variant="outline" size="sm" onClick={onCancelEdit}>
              取消
            </Button>
          </div>
        </Field>
      )}

      <div className="flex flex-wrap gap-2 pl-9">
        {!editing && (
          <Button variant="outline" size="sm" onClick={onEdit}>
            <Pencil data-icon="inline-start" />
            自定义合并
          </Button>
        )}
        <Button
          variant="outline"
          size="sm"
          disabled={arbitrationDisabled}
          onClick={onArbitrate}
        >
          {busy ? (
            <Loader2 data-icon="inline-start" className="animate-spin" />
          ) : segment.source === "escalated" ? (
            <RotateCcw data-icon="inline-start" />
          ) : (
            <Gavel data-icon="inline-start" />
          )}
          {segment.source === "escalated" ? "重新仲裁" : "发起仲裁"}
        </Button>
      </div>
    </section>
  )
}

function SegmentRow({
  segment,
  index,
}: {
  segment: FinalSegment
  index: number
}) {
  const meta = sourceMeta[segment.source]
  return (
    <div className="flex items-start gap-3 border-b px-3 py-2 text-sm last:border-b-0 hover:bg-muted/40">
      <span className="mt-0.5 w-6 shrink-0 text-right text-xs tabular-nums text-muted-foreground">
        {index + 1}
      </span>
      <span className="min-w-0 flex-1 whitespace-pre-wrap break-words">
        {segment.text || "(已删除)"}
      </span>
      <div className="flex shrink-0 items-center gap-2">
        <span
          className="hidden max-w-44 truncate text-xs text-muted-foreground md:inline"
          title={segment.from.join(", ")}
        >
          {segment.from.join(" · ")}
        </span>
        <span className="text-xs tabular-nums text-muted-foreground">
          {pct(segment.confidence)}
        </span>
        <Badge variant={meta.variant}>{meta.label}</Badge>
      </div>
    </div>
  )
}

function EngineOutput({ result }: { result: EngineResult }) {
  return (
    <div className="flex flex-col gap-3 rounded-md border p-4">
      <div className="flex items-center gap-2">
        <h3
          className="min-w-0 flex-1 truncate text-sm font-medium"
          title={result.agent}
        >
          {result.agent}
        </h3>
        <Badge variant={result.err ? "destructive" : "secondary"}>
          {result.err ? "失败" : `${(result.latency_ms / 1000).toFixed(1)}s`}
        </Badge>
      </div>
      {result.err ? (
        <p className="break-all text-xs text-destructive">{result.err}</p>
      ) : (
        <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words font-sans text-sm leading-6">
          {result.text || "(无输出)"}
        </pre>
      )}
    </div>
  )
}

function CopyButton({
  text,
  className,
  labeled = false,
}: {
  text: string
  className?: string
  labeled?: boolean
}) {
  const [status, setStatus] = useState<
    "idle" | "pending" | "success" | "error"
  >("idle")

  useEffect(() => setStatus("idle"), [text])

  const label =
    status === "pending"
      ? "正在复制"
      : status === "success"
        ? "已复制"
        : status === "error"
          ? "复制失败"
          : labeled
            ? "复制最终结果"
            : "复制"
  return (
    <div className={cn("flex items-center gap-2", className)}>
      {!labeled && status !== "idle" && (
        <Badge
          variant={status === "error" ? "destructive" : "secondary"}
          role="status"
        >
          {label}
        </Badge>
      )}
      <Tooltip>
        <TooltipTrigger asChild>
          <Button
            variant={
              status === "error"
                ? "destructive"
                : labeled
                  ? "default"
                  : "secondary"
            }
            size={labeled ? "sm" : "icon"}
            className={cn(!labeled && "size-8")}
            aria-label={label}
            disabled={status === "pending" || text.length === 0}
            onClick={async () => {
              setStatus("pending")
              try {
                await Promise.race([
                  navigator.clipboard.writeText(text),
                  new Promise<void>((_, reject) =>
                    window.setTimeout(
                      () => reject(new Error("复制超时")),
                      1000,
                    ),
                  ),
                ])
                setStatus("success")
              } catch {
                setStatus("error")
              }
              window.setTimeout(() => setStatus("idle"), 1800)
            }}
          >
            {status === "pending" ? (
              <Loader2
                data-icon={labeled ? "inline-start" : undefined}
                className="animate-spin"
              />
            ) : status === "success" ? (
              <Check data-icon={labeled ? "inline-start" : undefined} />
            ) : status === "error" ? (
              <AlertCircle data-icon={labeled ? "inline-start" : undefined} />
            ) : (
              <Copy data-icon={labeled ? "inline-start" : undefined} />
            )}
            {labeled && label}
          </Button>
        </TooltipTrigger>
        <TooltipContent>
          {status === "idle" && labeled ? "复制最新合并文本" : label}
        </TooltipContent>
      </Tooltip>
    </div>
  )
}

function buildDispute(segments: FinalSegment[], index: number): Dispute {
  let before = ""
  let after = ""
  for (let i = index - 1; i >= 0; i--) {
    if (segments[i].text.trim()) {
      before = segments[i].text
      break
    }
  }
  for (let i = index + 1; i < segments.length; i++) {
    if (segments[i].text.trim()) {
      after = segments[i].text
      break
    }
  }
  return {
    segment: index,
    before,
    after,
    candidates: segments[index].candidates ?? [],
  }
}
