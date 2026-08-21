import { useState, type FormEvent } from "react"
import { Loader2, LogIn, ScanText } from "lucide-react"

import { login, type AuthSession } from "@/lib/api"
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
  FieldError,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"

export function LoginPage({
  onLogin,
}: {
  onLogin: (session: AuthSession) => void
}) {
  const [username, setUsername] = useState("")
  const [password, setPassword] = useState("")
  const [error, setError] = useState("")
  const [pending, setPending] = useState(false)

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setPending(true)
    setError("")
    try {
      onLogin(await login(username, password))
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "登录失败")
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
            <CardTitle>登录</CardTitle>
            <CardDescription>使用 BetterOCR 账号进入工作台</CardDescription>
          </CardHeader>
          <form onSubmit={submit}>
            <CardContent>
              <FieldGroup>
                <Field data-invalid={Boolean(error)}>
                  <FieldLabel htmlFor="username">用户名</FieldLabel>
                  <Input
                    id="username"
                    name="username"
                    autoComplete="username"
                    autoFocus
                    required
                    value={username}
                    onChange={(event) => setUsername(event.target.value)}
                    aria-invalid={Boolean(error)}
                    disabled={pending}
                  />
                </Field>
                <Field data-invalid={Boolean(error)}>
                  <FieldLabel htmlFor="password">密码</FieldLabel>
                  <Input
                    id="password"
                    name="password"
                    type="password"
                    autoComplete="current-password"
                    required
                    value={password}
                    onChange={(event) => setPassword(event.target.value)}
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
                  <LogIn data-icon="inline-start" />
                )}
                {pending ? "正在登录" : "登录"}
              </Button>
            </CardFooter>
          </form>
        </Card>
      </main>
    </div>
  )
}
