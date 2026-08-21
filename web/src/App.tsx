import { useCallback, useEffect, useMemo, useState } from "react"
import {
  AlertCircle,
  ArrowRight,
  Bot,
  Layers3,
  Loader2,
  LogOut,
  Moon,
  ScanText,
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
import { AdminDialog } from "@/components/admin-dialog"
import { LoginPage } from "@/components/login-page"
import { SetupPage } from "@/components/setup-page"
import { TaskHistoryDialog } from "@/components/task-history-dialog"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"

const selectionStorageKey = "betterocr-model-selection"

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

  // —— 服务端模型目录与本地选择 ——
  const [cfg, setCfg] = useState<ServerConfig | null>(null)
  const [engines, setEngines] = useState<string[]>([])
  const [arbiter, setArbiter] = useState("")

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

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-10 border-b bg-background/80 backdrop-blur">
        <div className="mx-auto flex h-14 max-w-7xl items-center gap-3 px-4 md:px-6">
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
        </div>

        {error && (
          <Alert variant="destructive">
            <AlertCircle />
            <AlertTitle>模型配置加载失败</AlertTitle>
            <AlertDescription className="break-all">{error}</AlertDescription>
          </Alert>
        )}
        <DocumentWorkspace engines={engines} arbiter={arbiter} />
      </main>
    </div>
  )
}
