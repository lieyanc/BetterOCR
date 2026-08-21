import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import {
  AlertCircle,
  ArrowDown,
  ArrowLeft,
  ArrowUp,
  Braces,
  ChevronLeft,
  ChevronRight,
  FileImage,
  FileStack,
  FileText,
  FolderOpen,
  ListChecks,
  Loader2,
  Play,
  RotateCcw,
  ScanText,
  Square,
  Trash2,
  Upload,
  X,
} from "lucide-react"

import { ResultWorkbench } from "@/components/result-workbench"
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
import { Checkbox } from "@/components/ui/checkbox"
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty"
import { Field, FieldLabel } from "@/components/ui/field"
import { Progress } from "@/components/ui/progress"
import { ScrollArea, ScrollBar } from "@/components/ui/scroll-area"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import {
  cancelDocument,
  deleteDocument,
  deleteDocumentPage,
  documentExportURL,
  documentPageImageURL,
  fetchDocument,
  fetchDocumentDisputes,
  fetchDocumentPageResult,
  fetchDocuments,
  runDocument,
  streamDocumentProgress,
  updateDocumentPageOrder,
  updateDocumentPageResult,
  uploadDocument,
  type DocumentAgentProgress,
  type DocumentDisputeItem,
  type DocumentPageRecord,
  type DocumentProgressEvent,
  type DocumentProjectRecord,
  type Final,
} from "@/lib/api"
import { cn } from "@/lib/utils"

interface DocumentWorkspaceProps {
  engines: string[]
  arbiter: string
}

export function DocumentWorkspace({
  engines,
  arbiter,
}: DocumentWorkspaceProps) {
  const [projects, setProjects] = useState<DocumentProjectRecord[]>([])
  const [project, setProject] = useState<DocumentProjectRecord | null>(null)
  const [loadingProjects, setLoadingProjects] = useState(true)
  const [openingProject, setOpeningProject] = useState(false)
  const [selectedPageID, setSelectedPageID] = useState("")
  const [selectedResult, setSelectedResult] = useState<Final | null>(null)
  const [selectedImage, setSelectedImage] = useState<File | null>(null)
  const [loadingPage, setLoadingPage] = useState(false)
  const [savingResult, setSavingResult] = useState(false)
  const [view, setView] = useState<"page" | "audit">("page")
  const [disputes, setDisputes] = useState<DocumentDisputeItem[]>([])
  const [loadingAudit, setLoadingAudit] = useState(false)
  const [openDisputes, setOpenDisputes] = useState(false)
  const [uploading, setUploading] = useState(false)
  const [uploadProgress, setUploadProgress] = useState({ loaded: 0, total: 0 })
  const [dragging, setDragging] = useState(false)
  const [action, setAction] = useState("")
  const [autoArbitrate, setAutoArbitrate] = useState(true)
  const [error, setError] = useState("")
  const [liveProgress, setLiveProgress] =
    useState<DocumentProgressEvent | null>(null)
  const [progressConnection, setProgressConnection] = useState<
    "connecting" | "live" | "reconnecting"
  >("connecting")
  const fileInput = useRef<HTMLInputElement>(null)
  const uploadAbort = useRef<AbortController | null>(null)

  const loadProjects = useCallback(async () => {
    try {
      setProjects(await fetchDocuments())
    } catch (cause) {
      setError(errorMessage(cause))
    } finally {
      setLoadingProjects(false)
    }
  }, [])

  useEffect(() => {
    void loadProjects()
  }, [loadProjects])

  const mergeProject = useCallback((next: DocumentProjectRecord) => {
    setProject(next)
    setProjects((current) => {
      const summary = { ...next, pages: undefined }
      const exists = current.some((candidate) => candidate.id === next.id)
      const merged = exists
        ? current.map((candidate) =>
            candidate.id === next.id ? summary : candidate,
          )
        : [summary, ...current]
      return merged.sort(
        (left, right) =>
          new Date(right.updated_at).getTime() -
          new Date(left.updated_at).getTime(),
      )
    })
    setSelectedPageID((current) =>
      next.pages?.some((page) => page.id === current)
        ? current
        : (next.pages?.[0]?.id ?? ""),
    )
  }, [])

  const openProject = useCallback(
    async (id: string) => {
      setOpeningProject(true)
      setError("")
      setView("page")
      setOpenDisputes(false)
      try {
        mergeProject(await fetchDocument(id))
      } catch (cause) {
        setError(errorMessage(cause))
      } finally {
        setOpeningProject(false)
      }
    },
    [mergeProject],
  )

  useEffect(() => {
    if (!project) return
    let stopped = false
    const refresh = async () => {
      try {
        const next = await fetchDocument(project.id)
        if (!stopped) mergeProject(next)
      } catch (cause) {
        if (!stopped) setError(errorMessage(cause))
      }
    }
    const interval = window.setInterval(
      () => void refresh(),
      project.status === "preparing" || project.status === "processing"
        ? 1200
        : 4000,
    )
    return () => {
      stopped = true
      window.clearInterval(interval)
    }
  }, [mergeProject, project?.id, project?.status])

  useEffect(() => {
    if (!project || project.status !== "processing") {
      setLiveProgress(null)
      setProgressConnection("connecting")
      return
    }
    const documentID = project.id
    const controller = new AbortController()
    let stopped = false
    const subscribe = async () => {
      setProgressConnection("connecting")
      while (!stopped && !controller.signal.aborted) {
        try {
          await streamDocumentProgress(
            documentID,
            (progress) => {
              if (stopped) return
              setProgressConnection("live")
              setLiveProgress((current) =>
                !current || progress.sequence >= current.sequence
                  ? progress
                  : current,
              )
              if (
                ["completed", "failed", "cancelled"].includes(
                  progress.document_status,
                )
              ) {
                void fetchDocument(documentID)
                  .then(mergeProject)
                  .catch(() => undefined)
              }
            },
            controller.signal,
          )
        } catch (cause) {
          if (controller.signal.aborted) return
          setProgressConnection("reconnecting")
        }
        if (!stopped && !controller.signal.aborted) {
          setProgressConnection("reconnecting")
          await new Promise((resolve) => window.setTimeout(resolve, 800))
        }
      }
    }
    void subscribe()
    return () => {
      stopped = true
      controller.abort()
    }
  }, [mergeProject, project?.id, project?.status])

  const selectedIndex = Math.max(
    0,
    project?.pages?.findIndex((page) => page.id === selectedPageID) ?? 0,
  )
  const selectedPage = project?.pages?.[selectedIndex] ?? null
  const selectedPageProgress =
    liveProgress?.page_id === selectedPage?.id ? liveProgress : null

  useEffect(() => {
    setSelectedResult(null)
    setSelectedImage(null)
    if (!project || !selectedPage?.image_ready) return
    const controller = new AbortController()
    setLoadingPage(true)
    const load = async () => {
      try {
        const imageResponse = await fetch(
          documentPageImageURL(project.id, selectedPage.id),
          { signal: controller.signal },
        )
        if (!imageResponse.ok) throw new Error("读取当前页图像失败")
        const imageBlob = await imageResponse.blob()
        if (controller.signal.aborted) return
        setSelectedImage(
          new File([imageBlob], `${selectedPage.id}.jpg`, {
            type: imageBlob.type || "image/jpeg",
          }),
        )
        if (selectedPage.result_ready) {
          const result = await fetchDocumentPageResult(
            project.id,
            selectedPage.id,
          )
          if (!controller.signal.aborted) setSelectedResult(result)
        }
      } catch (cause) {
        if (!(cause instanceof DOMException && cause.name === "AbortError")) {
          setError(errorMessage(cause))
        }
      } finally {
        if (!controller.signal.aborted) setLoadingPage(false)
      }
    }
    void load()
    return () => controller.abort()
  }, [
    project?.id,
    selectedPage?.id,
    selectedPage?.image_ready,
    selectedPage?.result_ready,
    selectedPage?.revision,
  ])

  useEffect(() => {
    if (view !== "audit" || !project) return
    let stopped = false
    setLoadingAudit(true)
    fetchDocumentDisputes(project.id)
      .then((items) => {
        if (!stopped) setDisputes(items)
      })
      .catch((cause) => {
        if (!stopped) setError(errorMessage(cause))
      })
      .finally(() => {
        if (!stopped) setLoadingAudit(false)
      })
    return () => {
      stopped = true
    }
  }, [project?.id, project?.pending_disputes, project?.updated_at, view])

  const acceptFile = useCallback(
    async (file: File | null | undefined) => {
      if (!file || uploading) return
      const isPDF =
        file.type === "application/pdf" ||
        file.name.toLowerCase().endsWith(".pdf")
      if (!isPDF && !file.type.startsWith("image/")) {
        setError("仅支持 PDF、PNG、JPEG、WebP 和 GIF")
        return
      }
      if (engines.length === 0) {
        setError("请至少选择一个基础模型")
        return
      }
      const controller = new AbortController()
      uploadAbort.current = controller
      setUploading(true)
      setUploadProgress({ loaded: 0, total: file.size })
      setError("")
      try {
        const created = await uploadDocument(
          file,
          { engines, arbiter, autoArbitrate },
          (loaded, total) => setUploadProgress({ loaded, total }),
          controller.signal,
        )
        mergeProject(created)
        setView("page")
      } catch (cause) {
        if (!(cause instanceof DOMException && cause.name === "AbortError")) {
          setError(errorMessage(cause))
        }
      } finally {
        uploadAbort.current = null
        setUploading(false)
      }
    },
    [arbiter, autoArbitrate, engines, mergeProject, uploading],
  )

  useEffect(() => {
    const onPaste = (event: ClipboardEvent) => {
      if (uploading || action) return
      const item = Array.from(event.clipboardData?.items ?? []).find(
        (candidate) => candidate.type.startsWith("image/"),
      )
      if (item) void acceptFile(item.getAsFile())
    }
    window.addEventListener("paste", onPaste)
    return () => window.removeEventListener("paste", onPaste)
  }, [acceptFile, action, uploading])

  const runCurrent = async () => {
    if (!project || engines.length === 0) return
    setAction("run")
    setError("")
    try {
      mergeProject(
        await runDocument(project.id, { engines, arbiter, autoArbitrate }),
      )
    } catch (cause) {
      setError(errorMessage(cause))
    } finally {
      setAction("")
    }
  }

  const cancelCurrent = async () => {
    if (!project) return
    setAction("cancel")
    try {
      mergeProject(await cancelDocument(project.id))
    } catch (cause) {
      setError(errorMessage(cause))
    } finally {
      setAction("")
    }
  }

  const deleteCurrent = async () => {
    if (
      !project ||
      !window.confirm(`确定删除“${project.name}”及其全部页图和结果吗？`)
    )
      return
    setAction("delete")
    try {
      await deleteDocument(project.id)
      setProjects((current) =>
        current.filter((candidate) => candidate.id !== project.id),
      )
      setProject(null)
      setSelectedPageID("")
    } catch (cause) {
      setError(errorMessage(cause))
    } finally {
      setAction("")
    }
  }

  const movePage = async (direction: -1 | 1) => {
    if (!project?.pages || action) return
    const target = selectedIndex + direction
    if (target < 0 || target >= project.pages.length) return
    const pages = [...project.pages]
    ;[pages[selectedIndex], pages[target]] = [
      pages[target],
      pages[selectedIndex],
    ]
    setAction("order")
    try {
      mergeProject(
        await updateDocumentPageOrder(
          project.id,
          pages.map((page) => page.id),
        ),
      )
    } catch (cause) {
      setError(errorMessage(cause))
    } finally {
      setAction("")
    }
  }

  const removePage = async () => {
    if (!project || !selectedPage || action) return
    if (
      !window.confirm(`确定从项目中删除第 ${selectedPage.page_number} 页吗？`)
    )
      return
    setAction("remove-page")
    try {
      mergeProject(await deleteDocumentPage(project.id, selectedPage.id))
    } catch (cause) {
      setError(errorMessage(cause))
    } finally {
      setAction("")
    }
  }

  const savePageResult = (next: Final) => {
    if (!project || !selectedPage) return
    setSelectedResult(next)
    setSavingResult(true)
    updateDocumentPageResult(project.id, selectedPage.id, next)
      .then(mergeProject)
      .catch((cause) => setError(errorMessage(cause)))
      .finally(() => setSavingResult(false))
  }

  const openAuditItem = (item: DocumentDisputeItem) => {
    setSelectedPageID(item.page_id)
    setOpenDisputes(true)
    setView("page")
  }

  const busy = Boolean(action) || uploading

  return (
    <>
      <input
        ref={fileInput}
        type="file"
        accept="image/*,application/pdf,.pdf"
        className="hidden"
        onChange={(event) => {
          void acceptFile(event.target.files?.[0])
          event.target.value = ""
        }}
      />

      {!project ? (
        <ProjectHome
          projects={projects}
          loading={loadingProjects || openingProject}
          uploading={uploading}
          uploadProgress={uploadProgress}
          dragging={dragging}
          onDragging={setDragging}
          onUpload={(file) => void acceptFile(file)}
          onBrowse={() => fileInput.current?.click()}
          onCancelUpload={() => uploadAbort.current?.abort()}
          onOpen={(id) => void openProject(id)}
        />
      ) : (
        <Tabs
          value={view}
          onValueChange={(value) => setView(value as "page" | "audit")}
        >
          <section className="flex flex-col gap-3 border-b pb-4">
            <div className="flex flex-wrap items-start gap-3">
              <Button
                variant="ghost"
                size="icon"
                aria-label="返回项目列表"
                onClick={() => setProject(null)}
              >
                <ArrowLeft />
              </Button>
              <div className="flex size-9 shrink-0 items-center justify-center rounded-md border bg-muted">
                {project.source_type === "pdf" ? <FileStack /> : <FileImage />}
              </div>
              <div className="min-w-0 flex-1">
                <h1
                  className="truncate text-base font-semibold"
                  title={project.name}
                >
                  {project.name}
                </h1>
                <div className="mt-1 flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
                  <ProjectStatusBadge status={project.status} />
                  <span>{project.page_count || "-"} 页</span>
                  <span>{formatBytes(project.size_bytes)}</span>
                  {project.failed_pages > 0 && (
                    <span>{project.failed_pages} 页失败</span>
                  )}
                  {project.pending_disputes > 0 && (
                    <span>{project.pending_disputes} 处待审计</span>
                  )}
                </div>
              </div>
              <div className="flex flex-wrap items-center gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={busy}
                  onClick={() => fileInput.current?.click()}
                >
                  <Upload data-icon="inline-start" />
                  导入新项目
                </Button>
                <Button variant="outline" size="sm" asChild>
                  <a href={documentExportURL(project.id, "text")}>
                    <FileText data-icon="inline-start" />
                    导出全文
                  </a>
                </Button>
                <Button variant="outline" size="sm" asChild>
                  <a href={documentExportURL(project.id, "audit")}>
                    <Braces data-icon="inline-start" />
                    审计包
                  </a>
                </Button>
                <Tooltip>
                  <TooltipTrigger asChild>
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label="删除项目"
                      disabled={busy}
                      onClick={() => void deleteCurrent()}
                    >
                      {action === "delete" ? (
                        <Loader2 className="animate-spin" />
                      ) : (
                        <Trash2 />
                      )}
                    </Button>
                  </TooltipTrigger>
                  <TooltipContent>删除项目</TooltipContent>
                </Tooltip>
              </div>
            </div>

            {project.status === "preparing" && (
              <ProgressStatus
                label="服务端正在拆分并渲染页面"
                current={project.prepared_pages}
                total={project.page_count}
              />
            )}
            {project.status === "processing" && (
              <ProgressStatus
                label="服务端正在逐页识别"
                current={terminalPageCount(project.pages ?? [])}
                total={project.page_count}
              />
            )}

            <div className="flex flex-wrap items-center justify-between gap-3">
              <TabsList>
                <TabsTrigger value="page">逐页处理</TabsTrigger>
                <TabsTrigger value="audit">
                  统一审计
                  {project.pending_disputes > 0 && (
                    <Badge variant="secondary">
                      {project.pending_disputes}
                    </Badge>
                  )}
                </TabsTrigger>
              </TabsList>
              <div className="flex flex-wrap items-center gap-3">
                <Field orientation="horizontal" className="w-auto">
                  <Checkbox
                    id="document-auto-arbitrate"
                    checked={autoArbitrate}
                    disabled={project.status === "processing"}
                    onCheckedChange={(checked) =>
                      setAutoArbitrate(checked === true)
                    }
                  />
                  <FieldLabel htmlFor="document-auto-arbitrate">
                    自动仲裁
                  </FieldLabel>
                </Field>
                {project.status === "processing" ? (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={busy}
                    onClick={() => void cancelCurrent()}
                  >
                    {action === "cancel" ? (
                      <Loader2
                        data-icon="inline-start"
                        className="animate-spin"
                      />
                    ) : (
                      <Square data-icon="inline-start" />
                    )}
                    停止
                  </Button>
                ) : (
                  <Button
                    size="sm"
                    disabled={
                      busy ||
                      project.status === "preparing" ||
                      project.page_count === 0
                    }
                    onClick={() => void runCurrent()}
                  >
                    {action === "run" ? (
                      <Loader2
                        data-icon="inline-start"
                        className="animate-spin"
                      />
                    ) : project.processed_pages + project.failed_pages > 0 ? (
                      <RotateCcw data-icon="inline-start" />
                    ) : (
                      <Play data-icon="inline-start" />
                    )}
                    {project.processed_pages + project.failed_pages > 0
                      ? "继续 / 重试"
                      : "识别全部页面"}
                  </Button>
                )}
              </div>
            </div>
          </section>

          <TabsContent value="page" className="mt-2">
            {project.status === "preparing" && project.page_count === 0 ? (
              <Empty className="min-h-72 border">
                <EmptyHeader>
                  <EmptyMedia variant="icon">
                    <Loader2 className="animate-spin" />
                  </EmptyMedia>
                  <EmptyTitle>正在读取 PDF 目录</EmptyTitle>
                  <EmptyDescription>
                    源文件已安全落盘，服务端正在建立页面记录。
                  </EmptyDescription>
                </EmptyHeader>
              </Empty>
            ) : project.pages && project.pages.length > 0 ? (
              <div className="grid min-w-0 gap-4 lg:grid-cols-[14rem_minmax(0,1fr)]">
                <PageNavigator
                  pages={project.pages}
                  documentID={project.id}
                  selectedPageID={selectedPageID}
                  busy={
                    busy ||
                    project.status === "preparing" ||
                    project.status === "processing"
                  }
                  onSelect={(id) => {
                    setOpenDisputes(false)
                    setSelectedPageID(id)
                  }}
                  onMove={(direction) => void movePage(direction)}
                  onRemove={() => void removePage()}
                />
                {selectedPage && (
                  <div className="flex min-w-0 flex-col gap-4">
                    <div className="flex flex-wrap items-center justify-between gap-2">
                      <div className="flex items-center gap-2">
                        <Button
                          variant="outline"
                          size="icon"
                          aria-label="上一页"
                          disabled={selectedIndex <= 0}
                          onClick={() =>
                            setSelectedPageID(
                              project.pages?.[selectedIndex - 1]?.id ??
                                selectedPage.id,
                            )
                          }
                        >
                          <ChevronLeft />
                        </Button>
                        <span className="min-w-24 text-center text-sm font-medium tabular-nums">
                          第 {selectedPage.page_number} / {project.pages.length}{" "}
                          页
                        </span>
                        <Button
                          variant="outline"
                          size="icon"
                          aria-label="下一页"
                          disabled={selectedIndex >= project.pages.length - 1}
                          onClick={() =>
                            setSelectedPageID(
                              project.pages?.[selectedIndex + 1]?.id ??
                                selectedPage.id,
                            )
                          }
                        >
                          <ChevronRight />
                        </Button>
                      </div>
                      <div className="flex items-center gap-2 text-xs text-muted-foreground">
                        {savingResult && (
                          <>
                            <Loader2 className="size-3.5 animate-spin" />
                            正在保存审计
                          </>
                        )}
                        {selectedPage.duration_ms ? (
                          <span>
                            {(selectedPage.duration_ms / 1000).toFixed(1)}s
                          </span>
                        ) : null}
                        <PageStatusBadge page={selectedPage} />
                      </div>
                    </div>

                    {selectedPage.error && (
                      <Alert variant="destructive">
                        <AlertCircle />
                        <AlertTitle>本页处理失败</AlertTitle>
                        <AlertDescription className="break-all">
                          {selectedPage.error}
                        </AlertDescription>
                      </Alert>
                    )}

                    <div className="grid min-w-0 gap-4 xl:grid-cols-[minmax(18rem,0.8fr)_minmax(0,1.2fr)] xl:items-start">
                      <section className="overflow-hidden rounded-md border bg-muted/30 xl:sticky xl:top-20 xl:max-h-[calc(100vh-6rem)]">
                        {selectedPage.image_ready ? (
                          <img
                            src={documentPageImageURL(
                              project.id,
                              selectedPage.id,
                            )}
                            alt={`第 ${selectedPage.page_number} 页`}
                            className="mx-auto max-h-[72vh] w-full object-contain"
                          />
                        ) : (
                          <div className="flex min-h-64 items-center justify-center text-sm text-muted-foreground">
                            <Loader2 className="me-2 size-4 animate-spin" />
                            正在渲染本页
                          </div>
                        )}
                      </section>
                      <div className="min-w-0">
                        {loadingPage ? (
                          <Empty className="min-h-56 border">
                            <EmptyHeader>
                              <EmptyMedia variant="icon">
                                <Loader2 className="animate-spin" />
                              </EmptyMedia>
                              <EmptyTitle>正在读取当前页</EmptyTitle>
                            </EmptyHeader>
                          </Empty>
                        ) : selectedResult && selectedImage ? (
                          <ResultWorkbench
                            key={`${selectedPage.id}:${selectedPage.revision}`}
                            final={selectedResult}
                            image={selectedImage}
                            arbiter={arbiter}
                            initialTab={openDisputes ? "disputes" : undefined}
                            onChange={savePageResult}
                          />
                        ) : selectedPage.status === "processing" ||
                          selectedPageProgress?.status === "running" ? (
                          <DocumentLiveProgress
                            progress={selectedPageProgress}
                            connection={progressConnection}
                          />
                        ) : (
                          <Empty className="min-h-56 border">
                            <EmptyHeader>
                              <EmptyMedia variant="icon">
                                <ScanText />
                              </EmptyMedia>
                              <EmptyTitle>
                                {selectedPage.status === "failed"
                                  ? "本页可重新识别"
                                  : "本页等待识别"}
                              </EmptyTitle>
                            </EmptyHeader>
                          </Empty>
                        )}
                      </div>
                    </div>
                  </div>
                )}
              </div>
            ) : (
              <Empty className="min-h-64 border">
                <EmptyHeader>
                  <EmptyMedia variant="icon">
                    <AlertCircle />
                  </EmptyMedia>
                  <EmptyTitle>项目没有可用页面</EmptyTitle>
                  <EmptyDescription>
                    {project.error || "服务端未能建立页面记录。"}
                  </EmptyDescription>
                </EmptyHeader>
              </Empty>
            )}
          </TabsContent>

          <TabsContent value="audit" className="mt-2">
            <ProjectAudit
              project={project}
              disputes={disputes}
              loading={loadingAudit}
              onOpenPage={openAuditItem}
            />
          </TabsContent>
        </Tabs>
      )}

      {error && (
        <Alert variant="destructive">
          <AlertCircle />
          <AlertTitle>无法继续</AlertTitle>
          <AlertDescription className="break-all">{error}</AlertDescription>
          <Button
            variant="ghost"
            size="icon"
            aria-label="关闭错误"
            onClick={() => setError("")}
          >
            <X />
          </Button>
        </Alert>
      )}
    </>
  )
}

function ProjectHome({
  projects,
  loading,
  uploading,
  uploadProgress,
  dragging,
  onDragging,
  onUpload,
  onBrowse,
  onCancelUpload,
  onOpen,
}: {
  projects: DocumentProjectRecord[]
  loading: boolean
  uploading: boolean
  uploadProgress: { loaded: number; total: number }
  dragging: boolean
  onDragging: (value: boolean) => void
  onUpload: (file: File | undefined) => void
  onBrowse: () => void
  onCancelUpload: () => void
  onOpen: (id: string) => void
}) {
  const percent =
    uploadProgress.total > 0
      ? (uploadProgress.loaded / uploadProgress.total) * 100
      : 0
  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold">文档项目</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            服务端保存源文件、页图、识别进度和审计结果
          </p>
        </div>
        <Button disabled={uploading} onClick={onBrowse}>
          <Upload data-icon="inline-start" />
          导入 PDF 或图片
        </Button>
      </div>

      <section
        className={cn(
          "rounded-md border border-dashed transition-colors",
          dragging && "border-primary bg-primary/5",
        )}
        onDragOver={(event) => {
          event.preventDefault()
          if (!uploading) onDragging(true)
        }}
        onDragLeave={() => onDragging(false)}
        onDrop={(event) => {
          event.preventDefault()
          onDragging(false)
          onUpload(event.dataTransfer.files?.[0])
        }}
      >
        {uploading ? (
          <div className="mx-auto flex min-h-44 max-w-lg flex-col items-center justify-center gap-3 p-6">
            <Loader2 className="size-9 animate-spin text-muted-foreground" />
            <div className="flex w-full items-center justify-between gap-3 text-sm">
              <span className="font-medium">正在流式上传到服务端</span>
              <span className="tabular-nums text-muted-foreground">
                {percent.toFixed(1)}%
              </span>
            </div>
            <Progress value={percent} />
            <p className="text-xs tabular-nums text-muted-foreground">
              {formatBytes(uploadProgress.loaded)} /{" "}
              {formatBytes(uploadProgress.total)}
            </p>
            <Button variant="outline" size="sm" onClick={onCancelUpload}>
              取消上传
            </Button>
          </div>
        ) : (
          <Empty
            className="min-h-44 cursor-pointer border-0"
            onClick={onBrowse}
          >
            <EmptyHeader>
              <EmptyMedia variant="icon">
                <FileStack />
              </EmptyMedia>
              <EmptyTitle>拖入 PDF 或图片</EmptyTitle>
              <EmptyDescription>
                支持 1 GiB 以内 PDF 和数百页文档；页面由服务端逐页生成
              </EmptyDescription>
            </EmptyHeader>
            <EmptyContent>
              <Button variant="outline">
                <FolderOpen data-icon="inline-start" />
                选择文件
              </Button>
            </EmptyContent>
          </Empty>
        )}
      </section>

      <section className="flex flex-col gap-3">
        <div className="flex items-center justify-between gap-3">
          <h2 className="text-sm font-semibold">最近项目</h2>
          <span className="text-xs tabular-nums text-muted-foreground">
            {projects.length} 个
          </span>
        </div>
        {loading ? (
          <div className="flex min-h-32 items-center justify-center text-sm text-muted-foreground">
            <Loader2 className="me-2 size-4 animate-spin" />
            正在读取项目记录
          </div>
        ) : projects.length === 0 ? (
          <Empty className="min-h-36 border">
            <EmptyHeader>
              <EmptyMedia variant="icon">
                <FolderOpen />
              </EmptyMedia>
              <EmptyTitle>还没有文档项目</EmptyTitle>
            </EmptyHeader>
          </Empty>
        ) : (
          <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
            {projects.map((project) => (
              <Card
                key={project.id}
                className="cursor-pointer gap-4 py-4 transition-colors hover:bg-muted/40"
                onClick={() => onOpen(project.id)}
              >
                <CardHeader className="px-4">
                  <CardTitle className="flex min-w-0 items-center gap-2 text-sm">
                    {project.source_type === "pdf" ? (
                      <FileStack className="size-4 shrink-0" />
                    ) : (
                      <FileImage className="size-4 shrink-0" />
                    )}
                    <span className="truncate" title={project.name}>
                      {project.name}
                    </span>
                  </CardTitle>
                  <CardDescription>
                    {project.page_count || "-"} 页 ·{" "}
                    {formatBytes(project.size_bytes)} ·{" "}
                    {formatDate(project.updated_at)}
                  </CardDescription>
                  <CardAction>
                    <ProjectStatusBadge status={project.status} />
                  </CardAction>
                </CardHeader>
                <CardContent className="flex items-center gap-3 px-4">
                  <Progress
                    value={
                      project.page_count > 0
                        ? ((project.processed_pages + project.failed_pages) /
                            project.page_count) *
                          100
                        : 0
                    }
                    className="flex-1"
                  />
                  <span className="w-16 text-right text-xs tabular-nums text-muted-foreground">
                    {project.processed_pages}/{project.page_count || "-"}
                  </span>
                </CardContent>
              </Card>
            ))}
          </div>
        )}
      </section>
    </div>
  )
}

function PageNavigator({
  pages,
  documentID,
  selectedPageID,
  busy,
  onSelect,
  onMove,
  onRemove,
}: {
  pages: DocumentPageRecord[]
  documentID: string
  selectedPageID: string
  busy: boolean
  onSelect: (id: string) => void
  onMove: (direction: -1 | 1) => void
  onRemove: () => void
}) {
  const selectedIndex = pages.findIndex((page) => page.id === selectedPageID)
  return (
    <aside className="overflow-hidden rounded-md border lg:sticky lg:top-20 lg:self-start">
      <div className="flex items-center justify-between border-b px-3 py-2">
        <h2 className="text-sm font-semibold">页面</h2>
        <span className="text-xs tabular-nums text-muted-foreground">
          {pages.length}
        </span>
      </div>
      <ScrollArea className="h-36 lg:h-[calc(100vh-17rem)] lg:min-h-80 lg:max-h-[46rem]">
        <div className="flex w-max gap-2 p-2 lg:w-auto lg:flex-col">
          {pages.map((page) => (
            <button
              key={page.id}
              type="button"
              className={cn(
                "flex w-44 shrink-0 items-center gap-2 rounded-md border p-2 text-left transition-colors lg:w-full",
                page.id === selectedPageID
                  ? "border-primary bg-accent"
                  : "hover:bg-muted/60",
              )}
              aria-current={page.id === selectedPageID ? "page" : undefined}
              onClick={() => onSelect(page.id)}
            >
              {page.image_ready ? (
                <img
                  src={documentPageImageURL(documentID, page.id)}
                  alt=""
                  loading="lazy"
                  className="h-14 w-11 shrink-0 rounded-sm border bg-background object-cover"
                />
              ) : (
                <span className="flex h-14 w-11 shrink-0 items-center justify-center rounded-sm border bg-muted">
                  <Loader2 className="size-4 animate-spin" />
                </span>
              )}
              <span className="flex min-w-0 flex-1 flex-col gap-1">
                <span className="text-sm font-medium tabular-nums">
                  第 {page.page_number} 页
                </span>
                <span className="truncate text-xs text-muted-foreground">
                  {page.source_page !== page.page_number &&
                    `原第 ${page.source_page} 页 · `}
                  {pageStatusText(page)}
                </span>
              </span>
              {page.status === "processing" && (
                <Loader2 className="size-4 animate-spin" />
              )}
            </button>
          ))}
        </div>
        <ScrollBar orientation="horizontal" className="lg:hidden" />
      </ScrollArea>
      <div className="flex items-center justify-center gap-1 border-t p-2">
        <Tooltip>
          <TooltipTrigger asChild>
            <Button
              variant="ghost"
              size="icon"
              className="size-8"
              aria-label="页面前移"
              disabled={busy || selectedIndex <= 0}
              onClick={() => onMove(-1)}
            >
              <ArrowUp className="max-lg:-rotate-90" />
            </Button>
          </TooltipTrigger>
          <TooltipContent>页面前移</TooltipContent>
        </Tooltip>
        <Tooltip>
          <TooltipTrigger asChild>
            <Button
              variant="ghost"
              size="icon"
              className="size-8"
              aria-label="页面后移"
              disabled={
                busy || selectedIndex < 0 || selectedIndex >= pages.length - 1
              }
              onClick={() => onMove(1)}
            >
              <ArrowDown className="max-lg:-rotate-90" />
            </Button>
          </TooltipTrigger>
          <TooltipContent>页面后移</TooltipContent>
        </Tooltip>
        <Tooltip>
          <TooltipTrigger asChild>
            <Button
              variant="ghost"
              size="icon"
              className="size-8"
              aria-label="删除当前页"
              disabled={busy || pages.length <= 1}
              onClick={onRemove}
            >
              <Trash2 />
            </Button>
          </TooltipTrigger>
          <TooltipContent>删除当前页</TooltipContent>
        </Tooltip>
      </div>
    </aside>
  )
}

function ProjectAudit({
  project,
  disputes,
  loading,
  onOpenPage,
}: {
  project: DocumentProjectRecord
  disputes: DocumentDisputeItem[]
  loading: boolean
  onOpenPage: (item: DocumentDisputeItem) => void
}) {
  const completed =
    project.pages?.filter((page) => page.status === "completed").length ?? 0
  const averageConfidence = useMemo(() => {
    const results = project.pages?.filter((page) => page.result_ready) ?? []
    if (results.length === 0) return 0
    return (
      results.reduce((sum, page) => sum + (page.confidence ?? 0), 0) /
      results.length
    )
  }, [project.pages])
  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-2 overflow-hidden rounded-md border lg:grid-cols-4">
        <AuditMetric label="总页数" value={String(project.page_count)} />
        <AuditMetric
          label="已完成"
          value={`${completed}/${project.page_count}`}
        />
        <AuditMetric
          label="平均置信度"
          value={`${(averageConfidence * 100).toFixed(1)}%`}
        />
        <AuditMetric
          label="待处理争议"
          value={String(project.pending_disputes)}
          emphasized={project.pending_disputes > 0}
        />
      </div>
      <div>
        <h2 className="text-sm font-semibold">跨页争议清单</h2>
        <p className="mt-1 text-xs text-muted-foreground">
          结果保存在服务端，此处只加载待审计句段
        </p>
      </div>
      {loading ? (
        <div className="flex min-h-40 items-center justify-center text-sm text-muted-foreground">
          <Loader2 className="me-2 size-4 animate-spin" />
          正在汇总审计记录
        </div>
      ) : disputes.length === 0 ? (
        <Empty className="min-h-40 border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <ListChecks />
            </EmptyMedia>
            <EmptyTitle>
              {completed === project.page_count
                ? "整本文档没有待处理争议"
                : "完成识别后在此统一审计"}
            </EmptyTitle>
          </EmptyHeader>
        </Empty>
      ) : (
        <div className="overflow-hidden rounded-md border">
          {disputes.map((item) => (
            <div
              key={`${item.page_id}:${item.segment_index}`}
              className="flex flex-wrap items-start gap-3 border-b px-3 py-3 last:border-b-0 hover:bg-muted/40"
            >
              <div className="w-24 shrink-0">
                <p className="text-sm font-medium">第 {item.page_number} 页</p>
                <p className="text-xs text-muted-foreground">
                  句段 {item.segment_index + 1}
                </p>
              </div>
              <p className="min-w-48 flex-1 whitespace-pre-wrap break-words text-sm leading-6">
                {item.segment.text || "(当前判定为不存在)"}
              </p>
              <Badge variant="outline">待处理</Badge>
              <Button
                variant="outline"
                size="sm"
                onClick={() => onOpenPage(item)}
              >
                打开
              </Button>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

function DocumentLiveProgress({
  progress,
  connection,
}: {
  progress: DocumentProgressEvent | null
  connection: "connecting" | "live" | "reconnecting"
}) {
  const agents = progress?.agents ?? []
  const estimatedTokens = agents.reduce(
    (sum, agent) => sum + agent.estimated_tokens,
    0,
  )
  const aggregateTPS = agents.reduce((sum, agent) => sum + agent.tps, 0)
  const firstTokens = agents.filter((agent) => agent.first_token)
  const firstTokenMS =
    firstTokens.length > 0
      ? Math.min(...firstTokens.map((agent) => agent.ttft_ms ?? 0))
      : null
  const completed = progress?.completed_engines ?? 0
  const total = progress?.total_engines ?? 0
  const percent = total > 0 ? (completed / total) * 100 : 0

  return (
    <Card className="min-h-56">
      <CardHeader>
        <CardTitle>服务端正在识别本页</CardTitle>
        <CardDescription>{progressStageText(progress)}</CardDescription>
        <CardAction>
          <ProgressConnectionBadge connection={connection} />
        </CardAction>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div className="grid grid-cols-2 overflow-hidden rounded-md border lg:grid-cols-4">
          <LiveMetric
            label="本页耗时"
            value={formatMilliseconds(progress?.elapsed_ms ?? 0)}
          />
          <LiveMetric
            label="首个 Token 延迟"
            value={
              firstTokenMS === null
                ? "等待首个 Token"
                : formatMilliseconds(firstTokenMS)
            }
          />
          <LiveMetric
            label="估算 Token"
            value={estimatedTokens.toLocaleString("zh-CN")}
          />
          <LiveMetric
            label="估算 TPS"
            value={aggregateTPS > 0 ? aggregateTPS.toFixed(1) : "计算中"}
          />
        </div>

        <div className="flex items-center gap-3">
          <Progress value={percent} className="min-w-24 flex-1" />
          <span className="shrink-0 text-xs tabular-nums text-muted-foreground">
            基础模型 {completed} / {total || "-"}
          </span>
        </div>

        {agents.length === 0 ? (
          <div className="flex min-h-20 items-center justify-center text-sm text-muted-foreground">
            <Loader2 className="me-2 size-4 animate-spin" />
            等待本页任务启动
          </div>
        ) : (
          <div className="flex flex-col gap-4">
            {agents.map((agent) => (
              <LiveAgentProgress
                key={`${agent.stage}:${agent.agent}`}
                agent={agent}
              />
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function LiveAgentProgress({ agent }: { agent: DocumentAgentProgress }) {
  const preview = agent.output || agent.thinking
  return (
    <section className="flex min-w-0 flex-col gap-2 border-t pt-4 first:border-t-0 first:pt-0">
      <div className="flex min-w-0 flex-wrap items-center gap-2">
        <span
          className="min-w-0 flex-1 truncate text-sm font-medium"
          title={agent.agent}
        >
          {agent.agent}
        </span>
        <Badge variant={agentStatusVariant(agent.status)}>
          {(agent.status === "waiting" ||
            agent.status === "thinking" ||
            agent.status === "streaming") && (
            <Loader2 data-icon="inline-start" className="animate-spin" />
          )}
          {agentStatusText(agent)}
        </Badge>
        {agent.max_attempts > 1 && (
          <Badge variant="outline">
            {agent.attempt > 1 ? "重试" : "尝试"} {agent.attempt || 1}/
            {agent.max_attempts}
          </Badge>
        )}
        {agent.stage === "arbiter" && <Badge variant="outline">仲裁</Badge>}
      </div>
      <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs tabular-nums text-muted-foreground">
        <span>
          {agent.first_token
            ? `首个 Token ${formatMilliseconds(agent.ttft_ms ?? 0)}`
            : `等待首个 Token ${formatMilliseconds(agent.elapsed_ms)}`}
        </span>
        <span>估算 {agent.estimated_tokens} token</span>
        <span>
          {agent.tps > 0 ? `估算 ${agent.tps.toFixed(1)} TPS` : "TPS 计算中"}
        </span>
        <span>{agent.output_chars} 字符</span>
      </div>
      {preview && (
        <div className="min-w-0 rounded-md border bg-muted/30 p-3">
          <div className="mb-2 text-xs font-medium text-muted-foreground">
            {agent.output ? "实时正文" : "模型思考"}
          </div>
          <pre className="max-h-32 overflow-auto whitespace-pre-wrap break-words font-mono text-xs leading-5">
            {preview}
          </pre>
        </div>
      )}
      {agent.error && (
        <p className="break-all text-xs text-destructive">{agent.error}</p>
      )}
      {!agent.error && agent.last_error && (
        <p className="break-all text-xs text-muted-foreground">
          上次尝试失败：{agent.last_error}
        </p>
      )}
    </section>
  )
}

function ProgressConnectionBadge({
  connection,
}: {
  connection: "connecting" | "live" | "reconnecting"
}) {
  if (connection === "live") return <Badge variant="secondary">实时</Badge>
  return (
    <Badge variant="outline">
      <Loader2 data-icon="inline-start" className="animate-spin" />
      {connection === "reconnecting" ? "重连中" : "连接中"}
    </Badge>
  )
}

function LiveMetric({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex min-h-20 min-w-0 flex-col justify-center gap-1 border-b border-e p-3 even:border-e-0 lg:border-b-0 lg:even:border-e lg:last:border-e-0">
      <span className="text-xs text-muted-foreground">{label}</span>
      <span
        className="truncate text-base font-semibold tabular-nums"
        title={value}
      >
        {value}
      </span>
    </div>
  )
}

function progressStageText(progress: DocumentProgressEvent | null): string {
  if (!progress) return "正在连接进度流"
  switch (progress.stage) {
    case "loading":
      return "正在读取服务端页图"
    case "engine":
      return `基础模型并发识别中，已完成 ${progress.completed_engines} / ${progress.total_engines}`
    case "merge":
      return "基础模型已全部返回，正在对齐文本并定位分歧"
    case "arbiter":
      return "基础模型已全部返回，仲裁模型正在裁定分歧"
    case "saving":
      return "模型输出已完成，正在生成并保存最终结果"
    case "complete":
      return progress.error || "本页识别完成"
    default:
      return "等待服务端调度本页"
  }
}

function agentStatusText(agent: DocumentAgentProgress): string {
  switch (agent.status) {
    case "waiting":
      return "等待首个 Token"
    case "thinking":
      return "思考中"
    case "streaming":
      return "输出中"
    case "retrying":
      return "准备重试"
    case "completed":
      return "已完成"
    case "failed":
      return "失败"
  }
}

function agentStatusVariant(
  status: DocumentAgentProgress["status"],
): "default" | "secondary" | "outline" | "destructive" {
  if (status === "failed") return "destructive"
  if (status === "completed") return "secondary"
  if (status === "waiting" || status === "retrying") return "outline"
  return "default"
}

function ProgressStatus({
  label,
  current,
  total,
}: {
  label: string
  current: number
  total: number
}) {
  const percent = total > 0 ? (current / total) * 100 : 0
  return (
    <div className="flex items-center gap-3 rounded-md border px-3 py-2">
      <Loader2 className="size-4 shrink-0 animate-spin text-muted-foreground" />
      <span className="text-xs font-medium">{label}</span>
      <Progress value={percent} className="min-w-24 flex-1" />
      <span className="text-xs tabular-nums text-muted-foreground">
        {current} / {total || "-"}
      </span>
    </div>
  )
}

function AuditMetric({
  label,
  value,
  emphasized = false,
}: {
  label: string
  value: string
  emphasized?: boolean
}) {
  return (
    <div className="flex min-h-24 flex-col justify-center gap-1 border-b border-e p-4 even:border-e-0 lg:border-b-0 lg:even:border-e lg:last:border-e-0">
      <span className="text-xs text-muted-foreground">{label}</span>
      <span
        className={cn(
          "text-xl font-semibold tabular-nums",
          emphasized && "text-destructive",
        )}
      >
        {value}
      </span>
    </div>
  )
}

function ProjectStatusBadge({
  status,
}: {
  status: DocumentProjectRecord["status"]
}) {
  const labels: Record<DocumentProjectRecord["status"], string> = {
    preparing: "准备页面",
    ready: "待识别",
    processing: "识别中",
    completed: "已完成",
    failed: "失败",
    cancelled: "已停止",
  }
  if (status === "failed")
    return <Badge variant="destructive">{labels[status]}</Badge>
  if (status === "processing" || status === "preparing")
    return (
      <Badge>
        <Loader2 data-icon="inline-start" className="animate-spin" />
        {labels[status]}
      </Badge>
    )
  if (status === "completed")
    return <Badge variant="secondary">{labels[status]}</Badge>
  return <Badge variant="outline">{labels[status]}</Badge>
}

function PageStatusBadge({ page }: { page: DocumentPageRecord }) {
  if (page.status === "processing" || page.status === "preparing")
    return (
      <Badge>
        <Loader2 data-icon="inline-start" className="animate-spin" />
        {page.status === "processing" ? "识别中" : "准备中"}
      </Badge>
    )
  if (page.status === "failed") return <Badge variant="destructive">失败</Badge>
  if (page.status === "completed")
    return (
      <Badge variant="secondary">
        {(page.pending_disputes ?? 0) > 0 ? "待审计" : "已完成"}
      </Badge>
    )
  return <Badge variant="outline">未识别</Badge>
}

function pageStatusText(page: DocumentPageRecord): string {
  if (page.status === "completed")
    return (page.pending_disputes ?? 0) > 0
      ? `${page.pending_disputes} 处待审计`
      : "已完成"
  if (page.status === "processing") return "识别中"
  if (page.status === "preparing") return "生成页图"
  if (page.status === "failed") return "识别失败"
  return "等待识别"
}

function terminalPageCount(pages: DocumentPageRecord[]): number {
  return pages.filter(
    (page) => page.status === "completed" || page.status === "failed",
  ).length
}

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B"
  const units = ["B", "KiB", "MiB", "GiB"]
  const power = Math.min(
    Math.floor(Math.log(bytes) / Math.log(1024)),
    units.length - 1,
  )
  const value = bytes / 1024 ** power
  return `${value >= 10 || power === 0 ? value.toFixed(0) : value.toFixed(1)} ${units[power]}`
}

function formatDate(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime())
    ? "-"
    : date.toLocaleString("zh-CN", {
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
      })
}

function formatMilliseconds(milliseconds: number): string {
  if (!Number.isFinite(milliseconds) || milliseconds <= 0) return "0.0s"
  return `${(milliseconds / 1000).toFixed(milliseconds < 10_000 ? 1 : 0)}s`
}

function errorMessage(cause: unknown): string {
  return cause instanceof Error ? cause.message : String(cause)
}
