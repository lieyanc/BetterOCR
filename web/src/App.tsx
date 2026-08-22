import { useCallback, useEffect, useMemo, useState } from "react"
import {
  AlertCircle,
  ArrowRight,
  Bot,
  Layers3,
  Loader2,
  LogOut,
  Moon,
  ScanSearch,
  Sun,
} from "lucide-react"

import {
  fetchConfig,
  fetchSession,
  fetchSetupStatus,
  logout,
  type AuthSession,
  type ServerConfig,
} from "@/lib/api"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { DocumentWorkspace } from "@/components/document-workspace"
import { ModelConfigDialog } from "@/components/model-config-dialog"
import {
  AdminDialog,
  type AdminDialogTab,
} from "@/components/admin-dialog"
import { BrandHeader } from "@/components/brand-header"
import { LoginPage } from "@/components/login-page"
import { SetupPage } from "@/components/setup-page"
import { TaskHistoryDialog } from "@/components/task-history-dialog"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"

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

  const [error, setError] = useState("")
  const [adminDialogOpen, setAdminDialogOpen] = useState(false)
  const [adminDialogTab, setAdminDialogTab] =
    useState<AdminDialogTab>("users")

  // —— 服务端模型目录与本地选择 ——
  const [cfg, setCfg] = useState<ServerConfig | null>(null)
  const [engines, setEngines] = useState<string[]>([])
  const [arbiter, setArbiter] = useState("")
  const [duplicateChecker, setDuplicateChecker] = useState("")

  const loadConfig = useCallback(() => {
    fetchConfig()
      .then((c) => {
        setCfg(c)
        setEngines(c.engines)
        setArbiter(c.arbiter)
        setDuplicateChecker(c.duplicate_checker ?? "")
      })
      .catch((cause) => {
        setError(cause instanceof Error ? cause.message : "加载模型配置失败")
      })
  }, [])

  useEffect(() => {
    void loadConfig()
  }, [loadConfig])

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

  const applyModelSelection = (
    nextEngines: string[],
    nextArbiter: string,
    nextDuplicateChecker: string,
  ) => {
    setEngines(nextEngines)
    setArbiter(nextArbiter)
    setDuplicateChecker(nextDuplicateChecker)
  }

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-10 border-b bg-background/80 backdrop-blur">
        <div className="mx-auto flex h-14 max-w-7xl items-center gap-3 px-4 md:px-6">
          <BrandHeader
            tagline="全文识别 · 中文句段动态融合"
            compactOnMobile
            onVersionClick={
              session.user.role === "admin"
                ? () => {
                    setAdminDialogTab("update")
                    setAdminDialogOpen(true)
                  }
                : undefined
            }
            className="flex-1"
          />
          <div className="ms-auto flex items-center gap-1.5">
            <TaskHistoryDialog user={session.user} />
            {session.user.role === "admin" && (
              <AdminDialog
                currentUser={session.user}
                onSettingsChanged={() => void loadConfig()}
                open={adminDialogOpen}
                onOpenChange={setAdminDialogOpen}
                tab={adminDialogTab}
                onTabChange={setAdminDialogTab}
              />
            )}
            <ModelConfigDialog
              config={cfg}
              engines={engines}
              arbiter={arbiter}
              duplicateChecker={duplicateChecker}
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

      <main className="mx-auto flex max-w-7xl flex-col gap-4 p-4 md:p-6">
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
          <ArrowRight className="hidden size-4 shrink-0 text-muted-foreground sm:block" />
          <div className="flex min-w-0 basis-full items-center gap-2 sm:basis-auto">
            <ScanSearch className="size-4 shrink-0 text-muted-foreground" />
            <span className="text-xs font-medium text-muted-foreground">
              Fast Model
            </span>
            <Badge variant="outline" title={duplicateChecker || undefined}>
              {duplicateChecker
                ? (() => {
                    const m = modelIndex.get(duplicateChecker)
                    return m
                      ? `${m.provider} · ${m.alias}`
                      : duplicateChecker
                  })()
                : "未启用"}
            </Badge>
          </div>
        </div>

        {error && (
          <Alert variant="destructive">
            <AlertCircle />
            <AlertTitle>模型配置加载失败</AlertTitle>
            <AlertDescription className="break-all">{error}</AlertDescription>
          </Alert>
        )}
        <DocumentWorkspace
          engines={engines}
          arbiter={arbiter}
          duplicateChecker={duplicateChecker}
          onModelSelectionConsumed={
            session.user.role === "user" ? () => void loadConfig() : undefined
          }
        />
      </main>
    </div>
  )
}
