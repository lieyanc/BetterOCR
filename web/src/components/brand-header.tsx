import { useEffect, useState } from "react"
import { Github, ScanText } from "lucide-react"

import { fetchVersion, type VersionInfo } from "@/lib/api"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { cn } from "@/lib/utils"

export function BrandHeader({
  tagline,
  compactOnMobile = false,
  className,
}: {
  tagline: string
  compactOnMobile?: boolean
  className?: string
}) {
  const [version, setVersion] = useState<VersionInfo | null>(null)

  useEffect(() => {
    let cancelled = false
    fetchVersion()
      .then((info) => {
        if (!cancelled) setVersion(info)
      })
      .catch(() => {
        // Build metadata is supplementary; the rest of the app stays usable.
      })
    return () => {
      cancelled = true
    }
  }, [])

  const repo = version?.update_repo.trim().replace(/^\/+|\/+$/g, "")
  const repoURL = repo
    ? `https://github.com/${repo
        .split("/")
        .map((segment) => encodeURIComponent(segment))
        .join("/")}`
    : ""

  return (
    <div className={cn("flex min-w-0 items-center gap-3", className)}>
      <div className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary text-primary-foreground">
        <ScanText className="size-4.5" />
      </div>
      <div className="min-w-0">
        <p className="text-sm font-semibold leading-none">BetterOCR</p>
        <p className="mt-1 hidden truncate text-xs text-muted-foreground sm:block">
          {tagline}
        </p>
      </div>
      {version?.version && (
        <Badge variant="secondary" className="hidden sm:inline-flex">
          {version.version}
        </Badge>
      )}
      {repo && (
        <Tooltip>
          <TooltipTrigger asChild>
            <Button
              variant="ghost"
              size="sm"
              asChild
              className={cn(compactOnMobile && "hidden sm:inline-flex")}
            >
              <a
                href={repoURL}
                target="_blank"
                rel="noreferrer"
                aria-label={`在 GitHub 打开 ${repo}`}
              >
                <Github data-icon="inline-start" />
                <span className="hidden lg:inline">{repo}</span>
              </a>
            </Button>
          </TooltipTrigger>
          <TooltipContent>在 GitHub 打开 {repo}</TooltipContent>
        </Tooltip>
      )}
    </div>
  )
}
