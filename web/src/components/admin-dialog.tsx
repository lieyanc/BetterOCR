import { useEffect, useState, type FormEvent } from "react"
import {
  AlertCircle,
  CheckCircle2,
  Loader2,
  Pencil,
  RefreshCw,
  Save,
  Settings,
  ShieldCheck,
  Trash2,
  UserPlus,
  Users,
} from "lucide-react"

import {
  createUser,
  deleteUser,
  fetchAdminSettings,
  fetchUsers,
  updateAdminSettings,
  updateUser,
  type AdminSettings,
  type User,
  type UserRole,
} from "@/lib/api"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
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
import {
  Field,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { ScrollArea } from "@/components/ui/scroll-area"
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import { UpdatePanel } from "@/components/update-panel"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"

export type AdminDialogTab = "users" | "settings" | "update"

export function AdminDialog({
  currentUser,
  onSettingsChanged,
  open,
  onOpenChange,
  tab,
  onTabChange,
}: {
  currentUser: User
  onSettingsChanged: () => void
  open: boolean
  onOpenChange: (open: boolean) => void
  tab: AdminDialogTab
  onTabChange: (tab: AdminDialogTab) => void
}) {
  const [users, setUsers] = useState<User[]>([])
  const [settingsText, setSettingsText] = useState("")
  const [loading, setLoading] = useState(false)
  const [savingSettings, setSavingSettings] = useState(false)
  const [error, setError] = useState("")
  const [feedback, setFeedback] = useState("")
  const [editor, setEditor] = useState<User | "create" | null>(null)

  const load = async () => {
    setLoading(true)
    setError("")
    try {
      const [nextUsers, settings] = await Promise.all([
        fetchUsers(),
        fetchAdminSettings(),
      ])
      setUsers(nextUsers)
      setSettingsText(JSON.stringify(settings, null, 2))
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "加载管理数据失败")
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    if (open) void load()
  }, [open])

  const saveSettings = async () => {
    setError("")
    setFeedback("")
    setSavingSettings(true)
    try {
      const parsed = JSON.parse(settingsText) as AdminSettings
      const saved = await updateAdminSettings(parsed)
      setSettingsText(JSON.stringify(saved, null, 2))
      setFeedback("系统设置已保存")
      onSettingsChanged()
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "保存设置失败")
    } finally {
      setSavingSettings(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <Tooltip>
        <TooltipTrigger asChild>
          <DialogTrigger asChild>
            <Button
              variant="ghost"
              size="sm"
              aria-label="管理"
              onClick={() => onTabChange("users")}
            >
              <ShieldCheck data-icon="inline-start" />
              <span className="hidden sm:inline">管理</span>
            </Button>
          </DialogTrigger>
        </TooltipTrigger>
        <TooltipContent>系统管理</TooltipContent>
      </Tooltip>
      <DialogContent className="flex h-[min(780px,calc(100vh-2rem))] flex-col sm:max-w-5xl">
        <DialogHeader>
          <DialogTitle>系统管理</DialogTitle>
          <DialogDescription>用户权限与 BetterOCR 运行设置</DialogDescription>
        </DialogHeader>

        {error && (
          <Alert variant="destructive">
            <AlertCircle />
            <AlertTitle>操作未完成</AlertTitle>
            <AlertDescription className="break-all">{error}</AlertDescription>
          </Alert>
        )}
        {feedback && (
          <Alert>
            <CheckCircle2 />
            <AlertTitle>已保存</AlertTitle>
            <AlertDescription>{feedback}</AlertDescription>
          </Alert>
        )}

        <Tabs
          value={tab}
          onValueChange={(value) => onTabChange(value as AdminDialogTab)}
          className="min-h-0 flex-1"
        >
          <TabsList>
            <TabsTrigger value="users">
              <Users />
              用户
            </TabsTrigger>
            <TabsTrigger value="settings">
              <Settings />
              设置
            </TabsTrigger>
            <TabsTrigger value="update">
              <RefreshCw />
              更新
            </TabsTrigger>
          </TabsList>
          <TabsContent value="users" className="min-h-0 flex-1">
            <div className="flex h-full min-h-0 flex-col gap-3 pt-2">
              <div className="flex items-center justify-between gap-3">
                <p className="text-sm text-muted-foreground">
                  {users.length} 个用户
                </p>
                <Button size="sm" onClick={() => setEditor("create")}>
                  <UserPlus data-icon="inline-start" />
                  新建用户
                </Button>
              </div>
              {loading ? (
                <div className="flex flex-1 items-center justify-center text-sm text-muted-foreground">
                  <Loader2 className="me-2 size-4 animate-spin" />
                  正在加载
                </div>
              ) : users.length === 0 ? (
                <Empty>
                  <EmptyHeader>
                    <EmptyMedia variant="icon">
                      <Users />
                    </EmptyMedia>
                    <EmptyTitle>没有用户</EmptyTitle>
                    <EmptyDescription>
                      新建用户后会显示在这里。
                    </EmptyDescription>
                  </EmptyHeader>
                </Empty>
              ) : (
                <ScrollArea className="min-h-0 flex-1 rounded-md border">
                  <div className="divide-y">
                    {users.map((user) => (
                      <div
                        key={user.id}
                        className="flex min-h-16 items-center gap-3 px-4 py-3"
                      >
                        <div className="flex size-9 shrink-0 items-center justify-center rounded-md bg-muted text-sm font-semibold">
                          {user.username.slice(0, 1).toUpperCase()}
                        </div>
                        <div className="min-w-0 flex-1">
                          <div className="flex flex-wrap items-center gap-2">
                            <p className="truncate text-sm font-medium">
                              {user.username}
                            </p>
                            {user.id === currentUser.id && (
                              <Badge variant="outline">当前用户</Badge>
                            )}
                          </div>
                          <p className="mt-1 hidden text-xs text-muted-foreground sm:block">
                            创建于 {formatDate(user.created_at)}
                          </p>
                        </div>
                        <Badge
                          variant={
                            user.role === "admin" ? "default" : "secondary"
                          }
                        >
                          {user.role === "admin" ? "管理员" : "普通用户"}
                        </Badge>
                        {user.disabled && (
                          <Badge variant="destructive">已停用</Badge>
                        )}
                        <Button
                          variant="ghost"
                          size="icon"
                          aria-label={`编辑 ${user.username}`}
                          onClick={() => setEditor(user)}
                        >
                          <Pencil />
                        </Button>
                      </div>
                    ))}
                  </div>
                </ScrollArea>
              )}
            </div>
          </TabsContent>
          <TabsContent value="settings" className="min-h-0 flex-1">
            <div className="flex h-full min-h-0 flex-col gap-3 pt-2">
              <FieldGroup className="min-h-0 flex-1">
                <Field className="min-h-0 flex-1">
                  <FieldLabel htmlFor="system-settings">JSON 设置</FieldLabel>
                  <FieldDescription>
                    监听地址修改后需重启服务；API 密钥仅对管理员可见。
                  </FieldDescription>
                  <Textarea
                    id="system-settings"
                    className="min-h-0 flex-1 resize-none font-mono text-xs leading-5"
                    wrap="off"
                    spellCheck={false}
                    value={settingsText}
                    onChange={(event) => setSettingsText(event.target.value)}
                    disabled={loading || savingSettings}
                  />
                </Field>
              </FieldGroup>
              <div className="flex justify-end">
                <Button
                  onClick={() => void saveSettings()}
                  disabled={loading || savingSettings || !settingsText}
                >
                  {savingSettings ? (
                    <Loader2
                      data-icon="inline-start"
                      className="animate-spin"
                    />
                  ) : (
                    <Save data-icon="inline-start" />
                  )}
                  {savingSettings ? "正在保存" : "保存设置"}
                </Button>
              </div>
            </div>
          </TabsContent>
          <TabsContent value="update" className="min-h-0 flex-1">
            <UpdatePanel />
          </TabsContent>
        </Tabs>
      </DialogContent>

      <UserEditor
        value={editor}
        currentUser={currentUser}
        onOpenChange={(nextOpen) => {
          if (!nextOpen) setEditor(null)
        }}
        onSaved={(saved, created) => {
          setUsers((current) =>
            created
              ? [...current, saved]
              : current.map((user) => (user.id === saved.id ? saved : user)),
          )
          setEditor(null)
          setFeedback(created ? "用户已创建" : "用户已更新")
        }}
        onDeleted={(id) => {
          setUsers((current) => current.filter((user) => user.id !== id))
          setEditor(null)
          setFeedback("用户已删除")
        }}
      />
    </Dialog>
  )
}

function UserEditor({
  value,
  currentUser,
  onOpenChange,
  onSaved,
  onDeleted,
}: {
  value: User | "create" | null
  currentUser: User
  onOpenChange: (open: boolean) => void
  onSaved: (user: User, created: boolean) => void
  onDeleted: (id: string) => void
}) {
  const editing = value !== null && value !== "create" ? value : null
  const [username, setUsername] = useState("")
  const [password, setPassword] = useState("")
  const [role, setRole] = useState<UserRole>("user")
  const [disabled, setDisabled] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState("")
  const [confirmDelete, setConfirmDelete] = useState(false)

  useEffect(() => {
    setUsername(editing?.username ?? "")
    setPassword("")
    setRole(editing?.role ?? "user")
    setDisabled(editing?.disabled ?? false)
    setError("")
    setConfirmDelete(false)
  }, [value])

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setPending(true)
    setError("")
    try {
      if (editing) {
        const saved = await updateUser(editing.id, {
          username,
          password,
          role,
          disabled,
        })
        onSaved(saved, false)
      } else {
        const saved = await createUser({ username, password, role })
        onSaved(saved, true)
      }
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "保存用户失败")
    } finally {
      setPending(false)
    }
  }

  const remove = async () => {
    if (!editing) return
    if (!confirmDelete) {
      setConfirmDelete(true)
      return
    }
    setPending(true)
    setError("")
    try {
      await deleteUser(editing.id)
      onDeleted(editing.id)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "删除用户失败")
    } finally {
      setPending(false)
    }
  }

  const isSelf = editing?.id === currentUser.id
  return (
    <Dialog open={value !== null} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{editing ? "编辑用户" : "新建用户"}</DialogTitle>
          <DialogDescription>
            {editing ? editing.username : "创建可登录 BetterOCR 的账号"}
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} className="flex flex-col gap-5">
          <FieldGroup>
            <Field data-invalid={Boolean(error)}>
              <FieldLabel htmlFor="managed-username">用户名</FieldLabel>
              <Input
                id="managed-username"
                required
                value={username}
                onChange={(event) => setUsername(event.target.value)}
                aria-invalid={Boolean(error)}
                disabled={pending}
              />
            </Field>
            <Field data-invalid={Boolean(error)}>
              <FieldLabel htmlFor="managed-password">
                {editing ? "重置密码" : "密码"}
              </FieldLabel>
              <Input
                id="managed-password"
                type="password"
                autoComplete="new-password"
                required={!editing}
                minLength={8}
                value={password}
                onChange={(event) => setPassword(event.target.value)}
                aria-invalid={Boolean(error)}
                disabled={pending}
              />
              {editing && (
                <FieldDescription>留空则保持当前密码</FieldDescription>
              )}
            </Field>
            <Field>
              <FieldLabel htmlFor="managed-role">角色</FieldLabel>
              <Select
                value={role}
                onValueChange={(next) => setRole(next as UserRole)}
                disabled={pending || isSelf}
              >
                <SelectTrigger id="managed-role" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectGroup>
                    <SelectItem value="user">普通用户</SelectItem>
                    <SelectItem value="admin">管理员</SelectItem>
                  </SelectGroup>
                </SelectContent>
              </Select>
            </Field>
            {editing && (
              <Field orientation="horizontal" data-disabled={isSelf}>
                <Checkbox
                  id="managed-disabled"
                  checked={disabled}
                  onCheckedChange={(checked) => setDisabled(checked === true)}
                  disabled={pending || isSelf}
                />
                <FieldLabel htmlFor="managed-disabled">停用此用户</FieldLabel>
              </Field>
            )}
            <FieldError>{error}</FieldError>
          </FieldGroup>
          <DialogFooter className="sm:justify-between">
            {editing && !isSelf ? (
              <Button
                type="button"
                variant="destructive"
                onClick={() => void remove()}
                disabled={pending}
              >
                <Trash2 data-icon="inline-start" />
                {confirmDelete ? "确认删除" : "删除用户"}
              </Button>
            ) : (
              <span />
            )}
            <Button type="submit" disabled={pending}>
              {pending ? (
                <Loader2 data-icon="inline-start" className="animate-spin" />
              ) : (
                <Save data-icon="inline-start" />
              )}
              {pending ? "正在保存" : "保存"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function formatDate(value: string) {
  return new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium" }).format(
    new Date(value),
  )
}
