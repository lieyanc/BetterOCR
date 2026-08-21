import { useEffect, useMemo, useState } from "react"
import {
  AlertCircle,
  FileText,
  History,
  Loader2,
  RefreshCw,
} from "lucide-react"

import { cn } from "@/lib/utils"
import { fetchTasks, type TaskRecord, type User } from "@/lib/api"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Separator } from "@/components/ui/separator"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"

export function TaskHistoryDialog({ user }: { user: User }) {
  const [open, setOpen] = useState(false)
  const [tasks, setTasks] = useState<TaskRecord[]>([])
  const [selectedID, setSelectedID] = useState("")
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")

  const load = async () => {
    setLoading(true)
    setError("")
    try {
      const next = await fetchTasks()
      setTasks(next)
      setSelectedID((current) =>
        next.some((task) => task.id === current)
          ? current
          : (next[0]?.id ?? ""),
      )
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "加载任务记录失败")
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    if (open) void load()
  }, [open])

  const selected = useMemo(
    () => tasks.find((task) => task.id === selectedID) ?? null,
    [selectedID, tasks],
  )

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <Tooltip>
        <TooltipTrigger asChild>
          <DialogTrigger asChild>
            <Button variant="ghost" size="sm" aria-label="任务记录">
              <History data-icon="inline-start" />
              <span className="hidden sm:inline">任务记录</span>
            </Button>
          </DialogTrigger>
        </TooltipTrigger>
        <TooltipContent>任务记录</TooltipContent>
      </Tooltip>
      <DialogContent className="flex h-[min(760px,calc(100vh-2rem))] flex-col sm:max-w-6xl">
        <DialogHeader>
          <div className="flex items-start justify-between gap-4 pe-8">
            <div className="flex flex-col gap-2">
              <DialogTitle>
                {user.role === "admin" ? "全部任务记录" : "我的任务记录"}
              </DialogTitle>
              <DialogDescription>{tasks.length} 条记录</DialogDescription>
            </div>
            <Button
              variant="outline"
              size="icon"
              onClick={() => void load()}
              disabled={loading}
              aria-label="刷新任务记录"
            >
              <RefreshCw className={cn(loading && "animate-spin")} />
            </Button>
          </div>
        </DialogHeader>

        {error && (
          <Alert variant="destructive">
            <AlertCircle />
            <AlertTitle>无法加载记录</AlertTitle>
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}

        {loading && tasks.length === 0 ? (
          <div className="flex flex-1 items-center justify-center text-sm text-muted-foreground">
            <Loader2 className="me-2 size-4 animate-spin" />
            正在加载
          </div>
        ) : tasks.length === 0 ? (
          <Empty>
            <EmptyHeader>
              <EmptyMedia variant="icon">
                <FileText />
              </EmptyMedia>
              <EmptyTitle>暂无任务记录</EmptyTitle>
              <EmptyDescription>
                完成一次图片识别后，任务会出现在这里。
              </EmptyDescription>
            </EmptyHeader>
          </Empty>
        ) : (
          <div className="grid min-h-0 flex-1 grid-rows-[12rem_minmax(0,1fr)] gap-4 md:grid-cols-[18rem_minmax(0,1fr)] md:grid-rows-1">
            <ScrollArea className="min-h-48 rounded-md border">
              <div className="flex flex-col p-1.5">
                {tasks.map((task) => (
                  <button
                    key={task.id}
                    type="button"
                    className={cn(
                      "flex min-h-18 w-full flex-col gap-1.5 rounded-md px-3 py-2.5 text-left transition-colors hover:bg-accent",
                      selectedID === task.id &&
                        "bg-accent text-accent-foreground",
                    )}
                    onClick={() => setSelectedID(task.id)}
                  >
                    <span className="flex w-full items-center gap-2">
                      <span
                        className="min-w-0 flex-1 truncate text-sm font-medium"
                        title={task.filename}
                      >
                        {task.filename}
                      </span>
                      <TaskStatusBadge status={task.status} />
                    </span>
                    <span className="text-xs text-muted-foreground">
                      {formatDate(task.created_at)}
                    </span>
                    {user.role === "admin" && (
                      <span className="text-xs text-muted-foreground">
                        {task.username}
                      </span>
                    )}
                  </button>
                ))}
              </div>
            </ScrollArea>
            <TaskDetail task={selected} showOwner={user.role === "admin"} />
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}

function TaskDetail({
  task,
  showOwner,
}: {
  task: TaskRecord | null
  showOwner: boolean
}) {
  if (!task) return null
  return (
    <ScrollArea className="min-h-0 rounded-md border">
      <div className="flex flex-col gap-4 p-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <h3 className="break-all text-base font-semibold">
              {task.filename}
            </h3>
            <p className="mt-1 text-xs text-muted-foreground">
              {formatDate(task.created_at)}
            </p>
          </div>
          <TaskStatusBadge status={task.status} />
        </div>
        <div className="grid gap-3 text-sm sm:grid-cols-3">
          {showOwner && <Meta label="用户" value={task.username} />}
          <Meta
            label="耗时"
            value={
              task.duration_ms
                ? `${(task.duration_ms / 1000).toFixed(1)} 秒`
                : "-"
            }
          />
          <Meta label="基础模型" value={`${task.engines.length} 路`} />
        </div>
        <Separator />
        {task.error ? (
          <Alert variant="destructive">
            <AlertCircle />
            <AlertTitle>任务失败</AlertTitle>
            <AlertDescription className="break-all">
              {task.error}
            </AlertDescription>
          </Alert>
        ) : task.result ? (
          <>
            <div className="flex flex-wrap gap-2">
              <Badge variant="secondary">
                置信度 {(task.result.confidence * 100).toFixed(1)}%
              </Badge>
              <Badge variant="outline">
                {task.result.stats.segments} 个句段
              </Badge>
              {task.arbiter && <Badge variant="outline">已配置仲裁</Badge>}
            </div>
            <pre className="min-h-48 whitespace-pre-wrap break-words rounded-md bg-muted/50 p-4 font-sans text-sm leading-6">
              {task.result.text || "无文本结果"}
            </pre>
          </>
        ) : (
          <p className="text-sm text-muted-foreground">任务仍在处理中</p>
        )}
      </div>
    </ScrollArea>
  )
}

function Meta({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0">
      <p className="text-xs text-muted-foreground">{label}</p>
      <p className="mt-1 truncate font-medium" title={value}>
        {value}
      </p>
    </div>
  )
}

function TaskStatusBadge({ status }: { status: TaskRecord["status"] }) {
  if (status === "completed") return <Badge variant="secondary">已完成</Badge>
  if (status === "failed") return <Badge variant="destructive">失败</Badge>
  return <Badge variant="outline">处理中</Badge>
}

function formatDate(value: string) {
  return new Intl.DateTimeFormat("zh-CN", {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(value))
}
