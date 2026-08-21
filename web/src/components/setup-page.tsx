import { useState, type FormEvent } from "react"
import { Loader2, ScanText, UserRoundPlus } from "lucide-react"

import { initialize, type AuthSession } from "@/lib/api"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"

export function SetupPage({
  onInitialized,
}: {
  onInitialized: (session: AuthSession) => void
}) {
  const [username, setUsername] = useState("admin")
  const [password, setPassword] = useState("")
  const [confirmation, setConfirmation] = useState("")
  const [error, setError] = useState("")
  const [pending, setPending] = useState(false)

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    if (password !== confirmation) {
      setError("两次输入的密码不一致")
      return
    }
    setPending(true)
    setError("")
    try {
      onInitialized(await initialize(username, password))
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "初始化失败")
    } finally {
      setPending(false)
    }
  }

  return (
    <div className="flex min-h-screen flex-col bg-muted/30">
      <header className="border-b bg-background">
        <div className="mx-auto flex h-14 max-w-5xl items-center gap-3 px-4 md:px-6">
          <div className="flex size-8 items-center justify-center rounded-lg bg-primary text-primary-foreground">
            <ScanText className="size-4.5" />
          </div>
          <div>
            <p className="text-sm font-semibold leading-none">BetterOCR</p>
            <p className="mt-1 text-xs text-muted-foreground">
              多引擎 OCR 融合工作台
            </p>
          </div>
        </div>
      </header>
      <main className="flex flex-1 items-center justify-center p-4">
        <Card className="w-full max-w-sm rounded-lg">
          <CardHeader>
            <CardTitle>初始化 BetterOCR</CardTitle>
            <CardDescription>创建首个管理员账号</CardDescription>
          </CardHeader>
          <form onSubmit={submit}>
            <CardContent>
              <FieldGroup>
                <Field data-invalid={Boolean(error)}>
                  <FieldLabel htmlFor="setup-username">管理员用户名</FieldLabel>
                  <Input
                    id="setup-username"
                    name="username"
                    autoComplete="username"
                    required
                    autoFocus
                    value={username}
                    onChange={(event) => setUsername(event.target.value)}
                    aria-invalid={Boolean(error)}
                    disabled={pending}
                  />
                </Field>
                <Field data-invalid={Boolean(error)}>
                  <FieldLabel htmlFor="setup-password">密码</FieldLabel>
                  <Input
                    id="setup-password"
                    name="password"
                    type="password"
                    autoComplete="new-password"
                    required
                    minLength={8}
                    value={password}
                    onChange={(event) => setPassword(event.target.value)}
                    aria-invalid={Boolean(error)}
                    disabled={pending}
                  />
                  <FieldDescription>至少 8 个字符</FieldDescription>
                </Field>
                <Field data-invalid={Boolean(error)}>
                  <FieldLabel htmlFor="setup-confirmation">确认密码</FieldLabel>
                  <Input
                    id="setup-confirmation"
                    name="password-confirmation"
                    type="password"
                    autoComplete="new-password"
                    required
                    minLength={8}
                    value={confirmation}
                    onChange={(event) => setConfirmation(event.target.value)}
                    aria-invalid={Boolean(error)}
                    disabled={pending}
                  />
                  <FieldError>{error}</FieldError>
                </Field>
              </FieldGroup>
            </CardContent>
            <CardFooter className="mt-6">
              <Button className="w-full" type="submit" disabled={pending}>
                {pending ? (
                  <Loader2 data-icon="inline-start" className="animate-spin" />
                ) : (
                  <UserRoundPlus data-icon="inline-start" />
                )}
                {pending ? "正在初始化" : "创建管理员并进入"}
              </Button>
            </CardFooter>
          </form>
        </Card>
      </main>
    </div>
  )
}
