import { useCallback, useEffect, useRef, useState, type ReactNode } from "react"
import {
  AlertCircle,
  CheckCircle2,
  Download,
  FileText,
  GitCommit,
  Loader2,
  RefreshCw,
  RotateCw,
  Server,
  X,
} from "lucide-react"

import {
  applyUpdate,
  checkUpdate,
  dismissUpdate,
  fetchUpdateStatus,
  fetchVersion,
  type UpdateCheckResult,
  type UpdateState,
  type UpdateStatus,
  type VersionInfo,
} from "@/lib/api"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty"
import { Progress } from "@/components/ui/progress"
import { ScrollArea } from "@/components/ui/scroll-area"

const busyStates: UpdateState[] = ["checking", "downloading", "applying"]

const stateLabels: Record<UpdateState, string> = {
  idle: "空闲",
  checking: "正在检查",
  downloading: "正在下载",
  ready: "待重启",
  applying: "正在应用",
  failed: "失败",
}

const busyMessages: Partial<Record<UpdateState, string>> = {
  checking: "正在检查更新…",
  downloading: "正在下载并校验更新…",
  applying: "正在应用更新,请勿关闭服务…",
}

export function UpdatePanel() {
  const [version, setVersion] = useState<VersionInfo | null>(null)
  const [status, setStatus] = useState<UpdateStatus | null>(null)
  const [result, setResult] = useState<UpdateCheckResult | null>(null)
  const [pending, setPending] = useState<"check" | "apply" | "dismiss" | null>(
    null,
  )
  const [actionError, setActionError] = useState("")
  const [reconnecting, setReconnecting] = useState(false)
  const [confirmDismiss, setConfirmDismiss] = useState(false)

  const state = status?.state ?? "idle"
  const busy = busyStates.includes(state)
  const ready = state === "ready"

  // 轮询:忙时加速。更新期间进程会重启,轮询失败只当作"重连中",
  // 不清空已有状态,也不当成错误弹出来。
  useEffect(() => {
    let cancelled = false
    let timer: number | undefined
    const poll = async () => {
      try {
        const next = await fetchUpdateStatus()
        if (cancelled) return
        setStatus(next)
        setReconnecting(false)
      } catch {
        if (cancelled) return
        setReconnecting(true)
      }
      if (cancelled) return
      const current = busyStates.includes(state) ? 1500 : 5000
      timer = window.setTimeout(() => void poll(), current)
    }
    void poll()
    return () => {
      cancelled = true
      if (timer !== undefined) window.clearTimeout(timer)
    }
  }, [state])

  const loadVersion = useCallback(async () => {
    try {
      setVersion(await fetchVersion())
    } catch {
      // 版本信息只是展示用,失败时保留上一次的值。
    }
  }, [])

  useEffect(() => {
    void loadVersion()
  }, [loadVersion])

  // 更新装上后进程 exec 重启,旧前端资源还内嵌在旧二进制里:
  // 看到 current_version 变化就整页刷新,避免前后端版本错位。
  const currentVersion = status?.current_version
  const seenVersion = useRef<string | undefined>(undefined)
  useEffect(() => {
    if (!currentVersion) return
    if (seenVersion.current && seenVersion.current !== currentVersion) {
      window.location.reload()
      return
    }
    seenVersion.current = currentVersion
  }, [currentVersion])

  const runCheck = async () => {
    setPending("check")
    setActionError("")
    try {
      const next = await checkUpdate()
      setResult(next)
      setStatus(await fetchUpdateStatus())
    } catch (cause) {
      setActionError(cause instanceof Error ? cause.message : "检查更新失败")
    } finally {
      setPending(null)
    }
  }

  const runApply = async () => {
    setPending("apply")
    setActionError("")
    try {
      await applyUpdate()
      setStatus(await fetchUpdateStatus())
    } catch (cause) {
      setActionError(cause instanceof Error ? cause.message : "应用更新失败")
    } finally {
      setPending(null)
    }
  }

  const runDismiss = async () => {
    if (!confirmDismiss) {
      setConfirmDismiss(true)
      return
    }
    setPending("dismiss")
    setActionError("")
    try {
      await dismissUpdate()
      setResult(null)
      setStatus(await fetchUpdateStatus())
    } catch (cause) {
      setActionError(cause instanceof Error ? cause.message : "忽略更新失败")
    } finally {
      setPending(null)
      setConfirmDismiss(false)
    }
  }

  const hasUpdate = ready || Boolean(result?.has_update)
  const latest = status?.latest_version || result?.latest_version || ""
  const notes = status?.release_notes || result?.release_notes || ""
  const upToDate = !busy && !hasUpdate && result?.has_update === false
  const statusError = status?.error || result?.error || ""
  const progressValue = Math.max(
    0,
    Math.min(
      Math.round(
        (state === "downloading" ? status?.download_progress : status?.progress) ??
          0,
      ),
      100,
    ),
  )

  return (
    <div className="flex h-full min-h-0 flex-col gap-3 pt-2">
      <div className="flex flex-wrap items-center gap-2">
        <Badge
          variant={
            state === "failed"
              ? "destructive"
              : busy || ready
                ? "default"
                : "secondary"
          }
        >
          {stateLabels[state]}
        </Badge>
        {version?.update_enabled === false && (
          <span className="text-xs text-muted-foreground">
            后台自动检查已关闭(update.enabled),仍可在此手动更新
          </span>
        )}
        {reconnecting && (
          <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <Loader2 className="size-3 animate-spin" />
            正在等待服务重新响应
          </span>
        )}
        <div className="ms-auto flex items-center gap-1.5">
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void runCheck()}
            disabled={pending !== null || busy}
          >
            {pending === "check" ? (
              <Loader2 data-icon="inline-start" className="animate-spin" />
            ) : (
              <RefreshCw data-icon="inline-start" />
            )}
            {pending === "check" ? "检查中" : "检查更新"}
          </Button>
          {hasUpdate && (
            <Button
              size="sm"
              onClick={() => void runApply()}
              disabled={pending !== null || busy}
            >
              {pending === "apply" ? (
                <Loader2 data-icon="inline-start" className="animate-spin" />
              ) : ready ? (
                <RotateCw data-icon="inline-start" />
              ) : (
                <Download data-icon="inline-start" />
              )}
              {ready ? "重启应用" : "下载并更新"}
            </Button>
          )}
          {ready && (
            <Button
              variant="ghost"
              size="sm"
              onClick={() => void runDismiss()}
              disabled={pending !== null}
            >
              <X data-icon="inline-start" />
              {confirmDismiss ? "确认忽略" : "忽略"}
            </Button>
          )}
        </div>
      </div>

      {actionError && (
        <Alert variant="destructive">
          <AlertCircle />
          <AlertTitle>操作未完成</AlertTitle>
          <AlertDescription className="break-all">
            {actionError}
          </AlertDescription>
        </Alert>
      )}
      {statusError && !actionError && (
        <Alert variant="destructive">
          <AlertCircle />
          <AlertTitle>更新未完成</AlertTitle>
          <AlertDescription className="break-all">
            {statusError}
          </AlertDescription>
        </Alert>
      )}
      {upToDate && !statusError && (
        <Alert>
          <CheckCircle2 />
          <AlertTitle>已是最新版本</AlertTitle>
          <AlertDescription>当前通道没有需要安装的更新。</AlertDescription>
        </Alert>
      )}
      {ready && (
        <Alert>
          <RotateCw />
          <AlertTitle>{latest || "新版本"} 已下载并校验完成</AlertTitle>
          <AlertDescription>
            点击“重启应用”后服务会短暂中断,在途识别任务会先跑完再重启。
          </AlertDescription>
        </Alert>
      )}

      {(state === "downloading" || state === "applying") && (
        <div className="flex flex-col gap-1.5">
          <div className="flex items-center justify-between text-xs text-muted-foreground">
            <span>{busyMessages[state]}</span>
            <span className="tabular-nums">{progressValue}%</span>
          </div>
          <Progress value={progressValue} />
        </div>
      )}

      <div className="grid gap-3 sm:grid-cols-3">
        <InfoCard
          icon={<Server className="size-4" />}
          label="当前版本"
          value={status?.current_version || version?.version || "--"}
        />
        <InfoCard
          icon={<Download className="size-4" />}
          label="最新版本"
          value={latest || (upToDate ? "已是最新" : "--")}
        />
        <InfoCard
          icon={<GitCommit className="size-4" />}
          label="Commit"
          value={version?.commit ? version.commit.slice(0, 7) : "--"}
          title={version?.commit}
        />
      </div>

      <div className="grid gap-x-6 gap-y-2 rounded-md border p-4 text-sm sm:grid-cols-2">
        <InfoRow label="构建时间" value={version?.build_time} />
        <InfoRow
          label="更新通道"
          value={
            version?.update_channel === "dev"
              ? "dev(预发布,需手动确认重启)"
              : version?.update_channel === "stable"
                ? "stable(正式版,下载后自动重启)"
                : version?.update_channel
          }
        />
        <InfoRow
          label="更新来源"
          value={
            version?.update_source === "proxy"
              ? "代理镜像"
              : version?.update_source === "github"
                ? "GitHub 直连"
                : version?.update_source
          }
        />
        <InfoRow label="仓库" value={version?.update_repo} />
        <InfoRow label="上次检查" value={formatCheckedAt(status?.last_check)} />
      </div>

      {notes ? (
        <ScrollArea className="min-h-0 flex-1 rounded-md border">
          <pre className="whitespace-pre-wrap p-4 text-sm">{notes}</pre>
        </ScrollArea>
      ) : (
        <Empty className="min-h-0 flex-1">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <FileText />
            </EmptyMedia>
            <EmptyTitle>暂无发布说明</EmptyTitle>
            <EmptyDescription>
              GitHub 直连模式不读取 Release 说明;代理镜像模式会显示。
            </EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}
    </div>
  )
}

function InfoCard({
  icon,
  label,
  value,
  title,
}: {
  icon: ReactNode
  label: string
  value: string
  title?: string
}) {
  return (
    <div className="flex items-center gap-3 rounded-md border px-4 py-3">
      <span className="text-muted-foreground">{icon}</span>
      <div className="min-w-0">
        <div className="text-xs text-muted-foreground">{label}</div>
        <div className="truncate text-sm font-medium" title={title ?? value}>
          {value}
        </div>
      </div>
    </div>
  )
}

function InfoRow({ label, value }: { label: string; value?: string }) {
  return (
    <div className="flex min-w-0 items-baseline gap-2">
      <span className="shrink-0 text-xs text-muted-foreground">{label}</span>
      <span className="truncate font-medium" title={value}>
        {value || "--"}
      </span>
    </div>
  )
}

function formatCheckedAt(value?: string) {
  if (!value) return undefined
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) return value
  return new Intl.DateTimeFormat("zh-CN", {
    dateStyle: "short",
    timeStyle: "medium",
  }).format(parsed)
}
