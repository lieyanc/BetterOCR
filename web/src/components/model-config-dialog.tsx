import { useState } from "react"
import { KeyRound, Loader2, Settings2 } from "lucide-react"

import type { ModelAPI, ServerConfig } from "@/lib/api"
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
  Field,
  FieldContent,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
  FieldLegend,
  FieldSet,
  FieldTitle,
} from "@/components/ui/field"
import { ScrollArea } from "@/components/ui/scroll-area"
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectLabel,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"

const noArbiter = "__none__"

const apiLabels: Record<ModelAPI, string> = {
  "openai-chat-completions": "OpenAI Chat Completions",
  "openai-responses": "OpenAI Responses",
  "anthropic-messages": "Anthropic Messages",
}

function formatContext(context: number) {
  if (context >= 1_000_000)
    return `${(context / 1_000_000).toFixed(context % 1_000_000 ? 1 : 0)}M`
  if (context >= 1_000) return `${Math.round(context / 1_000)}K`
  return String(context)
}

interface ModelConfigDialogProps {
  config: ServerConfig | null
  engines: string[]
  arbiter: string
  duplicateChecker: string
  canSaveDefaults?: boolean
  onApply: (
    engines: string[],
    arbiter: string,
    duplicateChecker: string,
    saveAsDefault: boolean,
  ) => Promise<void> | void
}

export function ModelConfigDialog({
  config,
  engines,
  arbiter,
  duplicateChecker,
  canSaveDefaults = false,
  onApply,
}: ModelConfigDialogProps) {
  const [open, setOpen] = useState(false)
  const [draftEngines, setDraftEngines] = useState(engines)
  const [draftArbiter, setDraftArbiter] = useState(arbiter)
  const [draftDuplicateChecker, setDraftDuplicateChecker] =
    useState(duplicateChecker)
  const [saveAsDefault, setSaveAsDefault] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState("")

  const handleOpenChange = (next: boolean) => {
    if (next) {
      setDraftEngines(engines)
      setDraftArbiter(arbiter)
      setDraftDuplicateChecker(duplicateChecker)
      setSaveAsDefault(false)
      setError("")
    }
    setOpen(next)
  }

  const toggleEngine = (ref: string, checked: boolean) => {
    setDraftEngines((current) =>
      checked ? [...current, ref] : current.filter((item) => item !== ref),
    )
  }

  const apply = async () => {
    setSaving(true)
    setError("")
    try {
      await onApply(
        draftEngines,
        draftArbiter,
        draftDuplicateChecker,
        saveAsDefault,
      )
      setOpen(false)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "保存模型配置失败")
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <Tooltip>
        <TooltipTrigger asChild>
          <DialogTrigger asChild>
            <Button
              variant="outline"
              size="sm"
              disabled={!config}
              aria-label="模型配置"
            >
              <Settings2 data-icon="inline-start" />
              <span className="hidden sm:inline">模型配置</span>
            </Button>
          </DialogTrigger>
        </TooltipTrigger>
        <TooltipContent>模型配置</TooltipContent>
      </Tooltip>
      <DialogContent className="flex h-[min(46rem,calc(100dvh-2rem))] flex-col overflow-hidden sm:max-w-2xl">
        <DialogHeader className="shrink-0">
          <DialogTitle>模型配置</DialogTitle>
          <DialogDescription>
            从服务端配置的 Provider 中选择并发识别模型与分歧仲裁模型。
          </DialogDescription>
        </DialogHeader>

        <ScrollArea className="min-h-0 flex-1 overflow-hidden pr-3">
          <FieldSet className="gap-5">
            <FieldLegend className="sr-only">基础模型</FieldLegend>
            {config?.providers.map((provider) => (
              <section key={provider.id} className="flex flex-col gap-2.5">
                <div className="flex min-w-0 flex-wrap items-center gap-2">
                  <h3 className="text-sm font-semibold">{provider.alias}</h3>
                  <Badge
                    variant={provider.has_api_key ? "secondary" : "outline"}
                  >
                    <KeyRound />
                    {provider.has_api_key ? "密钥已配置" : "无密钥"}
                  </Badge>
                  <span
                    className="min-w-0 basis-full truncate text-left text-xs text-muted-foreground sm:flex-1 sm:basis-auto sm:text-right"
                    title={
                      provider.id === provider.alias
                        ? provider.base_url
                        : `${provider.id} · ${provider.base_url}`
                    }
                  >
                    {provider.id === provider.alias
                      ? provider.base_url
                      : `${provider.id} · ${provider.base_url}`}
                  </span>
                </div>
                <FieldGroup data-slot="checkbox-group" className="gap-2">
                  {provider.models.map((model) => {
                    const ref = `${provider.id}/${model.id}`
                    const checked = draftEngines.includes(ref)
                    return (
                      <FieldLabel
                        key={ref}
                        htmlFor={`engine-${ref}`}
                        className="cursor-pointer transition-colors hover:bg-muted/50"
                      >
                        <Field
                          orientation="horizontal"
                          className="items-center"
                        >
                          <Checkbox
                            id={`engine-${ref}`}
                            checked={checked}
                            disabled={saving}
                            onCheckedChange={(value) =>
                              toggleEngine(ref, value === true)
                            }
                          />
                          <div className="flex min-w-0 flex-1 flex-col gap-2 min-[421px]:flex-row min-[421px]:items-center">
                            <FieldContent className="min-w-0">
                              <FieldTitle className="truncate">
                                {model.alias}
                              </FieldTitle>
                              <FieldDescription
                                className="truncate text-xs"
                                title={model.id}
                              >
                                {model.id}
                              </FieldDescription>
                            </FieldContent>
                            <span className="flex shrink-0 flex-col items-start gap-1 min-[421px]:items-end">
                              <Badge variant="outline">
                                {apiLabels[model.api]}
                              </Badge>
                              <span className="text-xs tabular-nums text-muted-foreground">
                                {formatContext(model.context)} context
                              </span>
                            </span>
                          </div>
                        </Field>
                      </FieldLabel>
                    )
                  })}
                </FieldGroup>
              </section>
            ))}
          </FieldSet>
        </ScrollArea>

        <Field className="shrink-0">
          <FieldLabel htmlFor="arbiter-model">仲裁模型</FieldLabel>
          <Select
            value={draftArbiter || noArbiter}
            disabled={saving}
            onValueChange={(value) =>
              setDraftArbiter(value === noArbiter ? "" : value)
            }
          >
            <SelectTrigger id="arbiter-model" className="w-full">
              <SelectValue placeholder="选择仲裁模型" />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value={noArbiter}>不使用仲裁模型</SelectItem>
              </SelectGroup>
              {config?.providers.map((provider) => (
                <SelectGroup key={provider.id}>
                  <SelectLabel>{provider.alias}</SelectLabel>
                  {provider.models.map((model) => (
                    <SelectItem
                      key={`${provider.id}/${model.id}`}
                      value={`${provider.id}/${model.id}`}
                    >
                      {provider.alias} · {model.alias} · {apiLabels[model.api]}
                    </SelectItem>
                  ))}
                </SelectGroup>
              ))}
            </SelectContent>
          </Select>
        </Field>

        <Field className="shrink-0">
          <FieldLabel htmlFor="duplicate-checker-model">
            Fast Model
          </FieldLabel>
          <Select
            value={draftDuplicateChecker || noArbiter}
            disabled={saving}
            onValueChange={(value) =>
              setDraftDuplicateChecker(value === noArbiter ? "" : value)
            }
          >
            <SelectTrigger id="duplicate-checker-model" className="w-full">
              <SelectValue placeholder="选择 Fast Model" />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value={noArbiter}>不使用 Fast Model</SelectItem>
              </SelectGroup>
              {config?.providers.map((provider) => (
                <SelectGroup key={provider.id}>
                  <SelectLabel>{provider.alias}</SelectLabel>
                  {provider.models.map((model) => (
                    <SelectItem
                      key={`${provider.id}/${model.id}`}
                      value={`${provider.id}/${model.id}`}
                    >
                      {provider.alias} · {model.alias} · {apiLabels[model.api]}
                    </SelectItem>
                  ))}
                </SelectGroup>
              ))}
            </SelectContent>
          </Select>
        </Field>

        <DialogFooter className="shrink-0 sm:items-end sm:justify-between">
          <div className="flex min-w-0 flex-1 flex-col gap-2">
            <p className="text-xs text-muted-foreground">
              已选 {draftEngines.length} 个基础模型
            </p>
            {canSaveDefaults && (
              <Field orientation="horizontal" className="w-fit">
                <Checkbox
                  id="save-model-selection-defaults"
                  checked={saveAsDefault}
                  onCheckedChange={(checked) =>
                    setSaveAsDefault(checked === true)
                  }
                  disabled={saving}
                />
                <FieldLabel htmlFor="save-model-selection-defaults">
                  写入服务端配置并设为用户默认
                </FieldLabel>
              </Field>
            )}
            <FieldError>{error}</FieldError>
          </div>
          <div className="flex gap-2">
            <Button
              variant="outline"
              disabled={saving}
              onClick={() => {
                setDraftEngines(config?.engines ?? [])
                setDraftArbiter(config?.arbiter ?? "")
                setDraftDuplicateChecker(config?.duplicate_checker ?? "")
              }}
            >
              恢复默认
            </Button>
            <Button
              disabled={draftEngines.length === 0 || saving}
              onClick={() => void apply()}
            >
              {saving && (
                <Loader2 data-icon="inline-start" className="animate-spin" />
              )}
              {saving
                ? "正在保存"
                : saveAsDefault
                  ? "保存并应用"
                  : "应用"}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
