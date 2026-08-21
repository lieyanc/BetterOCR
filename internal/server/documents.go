package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lieyanc/BetterOCR/internal/arbiter"
	"github.com/lieyanc/BetterOCR/internal/database"
	"github.com/lieyanc/BetterOCR/internal/documents"
	"github.com/lieyanc/BetterOCR/internal/model"
	"github.com/lieyanc/BetterOCR/internal/pipeline"
)

const (
	maxDocumentBytes      = int64(1 << 30)
	maxDocumentImageBytes = int64(32 << 20)
	maxDocumentPages      = 5000
	maxResultBytes        = int64(32 << 20)
)

type documentJobKind string

const (
	documentJobPrepare documentJobKind = "prepare"
	documentJobProcess documentJobKind = "process"
)

type documentJob struct {
	id   string
	kind documentJobKind
}

type documentManager struct {
	server *Server
	store  *database.Store
	root   string

	ctx    context.Context
	jobs   chan documentJob
	mu     sync.Mutex
	queued map[string]bool
	runs   map[string]context.CancelFunc

	rendererOnce sync.Once
	renderer     documents.PageRenderer
	rendererErr  error
}

func (s *Server) getDocumentManager() (*documentManager, error) {
	s.documentOnce.Do(func() {
		if s.Store == nil {
			s.documentInitErr = errors.New("文档项目需要启用用户数据库")
			return
		}
		root := strings.TrimSpace(s.DocumentRoot)
		if root == "" {
			root = filepath.Join(s.Store.Directory(), "documents")
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			s.documentInitErr = fmt.Errorf("创建文档存储目录失败: %w", err)
			return
		}
		manager := &documentManager{
			server: s, store: s.Store, root: root, ctx: context.Background(),
			jobs: make(chan documentJob, 128), queued: map[string]bool{},
			runs: map[string]context.CancelFunc{}, renderer: s.PDFRenderer,
		}
		s.documentManager = manager
		go manager.worker()
		recovery, err := s.Store.RecoverDocuments()
		if err != nil {
			s.documentInitErr = fmt.Errorf("恢复文档任务失败: %w", err)
			return
		}
		for _, id := range recovery.Prepare {
			_ = manager.enqueue(documentJob{id: id, kind: documentJobPrepare})
		}
		for _, id := range recovery.Process {
			_ = manager.enqueue(documentJob{id: id, kind: documentJobProcess})
		}
	})
	return s.documentManager, s.documentInitErr
}

func (m *documentManager) pdfRenderer() (documents.PageRenderer, error) {
	m.rendererOnce.Do(func() {
		if m.renderer != nil {
			return
		}
		m.renderer, m.rendererErr = documents.NewPDFiumRenderer(m.ctx)
	})
	return m.renderer, m.rendererErr
}

func (m *documentManager) enqueue(job documentJob) error {
	key := string(job.kind) + ":" + job.id
	m.mu.Lock()
	if m.queued[key] {
		m.mu.Unlock()
		return nil
	}
	m.queued[key] = true
	m.mu.Unlock()
	select {
	case m.jobs <- job:
		return nil
	default:
		m.mu.Lock()
		delete(m.queued, key)
		m.mu.Unlock()
		return errors.New("文档后台队列已满,请稍后重试")
	}
}

func (m *documentManager) cancel(id string) {
	m.mu.Lock()
	cancel := m.runs[id]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *documentManager) worker() {
	for job := range m.jobs {
		ctx, cancel := context.WithCancel(m.ctx)
		m.mu.Lock()
		m.runs[job.id] = cancel
		m.mu.Unlock()

		switch job.kind {
		case documentJobPrepare:
			m.prepare(ctx, job.id)
		case documentJobProcess:
			m.process(ctx, job.id)
		}

		cancel()
		m.mu.Lock()
		delete(m.runs, job.id)
		delete(m.queued, string(job.kind)+":"+job.id)
		m.mu.Unlock()
	}
}

func (m *documentManager) projectDir(documentID string) string {
	return filepath.Join(m.root, documentID)
}

func (m *documentManager) sourcePath(document database.DocumentProject) string {
	name := "source.img"
	if document.SourceType == "pdf" {
		name = "source.pdf"
	}
	return filepath.Join(m.projectDir(document.ID), name)
}

func (m *documentManager) pageImagePath(document database.DocumentProject, page database.DocumentPage) string {
	extension := ".img"
	if document.SourceType == "pdf" {
		extension = ".jpg"
	}
	return filepath.Join(m.projectDir(document.ID), "pages", page.ID+extension)
}

func (m *documentManager) resultPath(documentID, pageID string) string {
	return filepath.Join(m.projectDir(documentID), "results", pageID+".json")
}

func (m *documentManager) prepare(ctx context.Context, id string) {
	document, ok := m.store.Document(id)
	if !ok || document.Status == database.DocumentCancelled {
		return
	}
	if document.SourceType == "image" {
		if err := m.prepareImage(ctx, document); err != nil {
			m.failDocument(id, ctx, err)
		}
		return
	}
	renderer, err := m.pdfRenderer()
	if err != nil {
		m.failDocument(id, ctx, err)
		return
	}
	err = renderer.Render(
		ctx,
		m.sourcePath(document),
		func(pageCount int) error {
			if pageCount > maxDocumentPages {
				return fmt.Errorf("PDF 共 %d 页,超过服务端上限 %d 页", pageCount, maxDocumentPages)
			}
			_, err := m.store.MutateDocument(id, func(current *database.DocumentProject) error {
				if current.Status == database.DocumentCancelled {
					return context.Canceled
				}
				if len(current.Pages) == 0 {
					current.Pages = make([]database.DocumentPage, pageCount)
					now := time.Now().UTC()
					for index := range current.Pages {
						current.Pages[index] = database.DocumentPage{
							ID: fmt.Sprintf("page-%06d", index+1), SourcePage: index + 1,
							PageNumber: index + 1, Status: database.PagePreparing, UpdatedAt: now,
						}
					}
				} else if len(current.Pages) != pageCount {
					return fmt.Errorf("PDF 页数从 %d 变为 %d,无法继续恢复", len(current.Pages), pageCount)
				}
				current.Status = database.DocumentPreparing
				current.Error = ""
				return nil
			})
			return err
		},
		func(pageIndex int) bool {
			current, ok := m.store.Document(id)
			if !ok || pageIndex >= len(current.Pages) {
				return false
			}
			page := current.Pages[pageIndex]
			if !page.ImageReady {
				return true
			}
			_, err := os.Stat(m.pageImagePath(current, page))
			return err != nil
		},
		func(pageIndex int, rendered image.Image) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			current, ok := m.store.Document(id)
			if !ok || pageIndex >= len(current.Pages) {
				return errors.New("页面记录在渲染期间被移除")
			}
			page := current.Pages[pageIndex]
			if err := writeAtomic(m.pageImagePath(current, page), 0o600, func(writer io.Writer) error {
				return jpeg.Encode(writer, rendered, &jpeg.Options{Quality: 90})
			}); err != nil {
				return fmt.Errorf("保存第 %d 页图像失败: %w", pageIndex+1, err)
			}
			_, err := m.store.MutateDocument(id, func(next *database.DocumentProject) error {
				pageIndex := slices.IndexFunc(next.Pages, func(candidate database.DocumentPage) bool { return candidate.ID == page.ID })
				if pageIndex < 0 {
					return errors.New("页面记录不存在")
				}
				next.Pages[pageIndex].ImageReady = true
				next.Pages[pageIndex].Status = database.PageQueued
				next.Pages[pageIndex].Error = ""
				next.Pages[pageIndex].UpdatedAt = time.Now().UTC()
				return nil
			})
			return err
		},
	)
	if err != nil {
		m.failDocument(id, ctx, err)
		return
	}
	m.finishPreparation(id)
}

func (m *documentManager) prepareImage(ctx context.Context, document database.DocumentProject) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := m.store.MutateDocument(document.ID, func(current *database.DocumentProject) error {
		if current.Status == database.DocumentCancelled {
			return context.Canceled
		}
		if len(current.Pages) == 0 {
			current.Pages = []database.DocumentPage{{
				ID: "page-000001", SourcePage: 1, PageNumber: 1,
				Status: database.PagePreparing, UpdatedAt: time.Now().UTC(),
			}}
		}
		return nil
	})
	if err != nil {
		return err
	}
	page := current.Pages[0]
	target := m.pageImagePath(current, page)
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		if err := linkOrCopy(m.sourcePath(current), target); err != nil {
			return fmt.Errorf("保存图片页失败: %w", err)
		}
	} else if err != nil {
		return err
	}
	_, err = m.store.MutateDocument(document.ID, func(next *database.DocumentProject) error {
		next.Pages[0].ImageReady = true
		next.Pages[0].Status = database.PageQueued
		next.Pages[0].UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return err
	}
	m.finishPreparation(document.ID)
	return nil
}

func (m *documentManager) finishPreparation(id string) {
	_, _ = m.store.MutateDocument(id, func(document *database.DocumentProject) error {
		if document.Status == database.DocumentCancelled {
			return nil
		}
		document.Status = database.DocumentReady
		document.Error = ""
		return nil
	})
}

func (m *documentManager) failDocument(id string, ctx context.Context, cause error) {
	_, _ = m.store.MutateDocument(id, func(document *database.DocumentProject) error {
		if document.Status == database.DocumentCancelled || errors.Is(cause, context.Canceled) || ctx.Err() != nil {
			document.Status = database.DocumentCancelled
			document.Error = ""
			return nil
		}
		document.Status = database.DocumentFailed
		document.Error = cause.Error()
		return nil
	})
}

func (m *documentManager) process(ctx context.Context, id string) {
	document, ok := m.store.Document(id)
	if !ok || document.Status == database.DocumentCancelled || document.PreparedPages == 0 {
		return
	}
	engines, arbiterModel, err := m.resolveModels(document)
	if err != nil {
		m.failDocument(id, ctx, err)
		return
	}
	document, err = m.store.MutateDocument(id, func(current *database.DocumentProject) error {
		if current.Status == database.DocumentCancelled {
			return context.Canceled
		}
		current.Status = database.DocumentProcessing
		current.CompletedAt = nil
		current.Error = ""
		return nil
	})
	if err != nil {
		return
	}

	for _, pageSnapshot := range document.Pages {
		if err := ctx.Err(); err != nil {
			m.failDocument(id, ctx, err)
			return
		}
		current, ok := m.store.Document(id)
		if !ok || current.Status == database.DocumentCancelled {
			return
		}
		pageIndex := slices.IndexFunc(current.Pages, func(page database.DocumentPage) bool { return page.ID == pageSnapshot.ID })
		if pageIndex < 0 {
			continue
		}
		page := current.Pages[pageIndex]
		if !page.ImageReady || page.Status == database.PageCompleted {
			continue
		}
		_, err := m.store.MutateDocument(id, func(next *database.DocumentProject) error {
			index := slices.IndexFunc(next.Pages, func(candidate database.DocumentPage) bool { return candidate.ID == page.ID })
			if index < 0 {
				return errors.New("页面记录不存在")
			}
			next.Pages[index].Status = database.PageProcessing
			next.Pages[index].Error = ""
			next.Pages[index].UpdatedAt = time.Now().UTC()
			return nil
		})
		if err != nil {
			continue
		}
		started := time.Now()
		pageImage, readErr := os.ReadFile(m.pageImagePath(current, page))
		if readErr != nil {
			m.failPage(id, page.ID, readErr, time.Since(started), false)
			continue
		}
		pageContext, cancel := context.WithTimeout(ctx, m.server.timeout())
		final, runErr := pipeline.Run(pageContext, pipeline.Config{
			Engines: engines, Arbiter: arbiterModel,
			DeferArbitration: !document.AutoArbitrate,
			HTTPClient:       m.server.HTTPClient,
		}, pageImage)
		cancel()
		if runErr != nil {
			if ctx.Err() != nil {
				m.failDocument(id, ctx, ctx.Err())
				return
			}
			m.failPage(id, page.ID, runErr, time.Since(started), false)
			continue
		}
		allFailed := final.Stats.Engines > 0 && final.Stats.FailedEngines == final.Stats.Engines
		if err := writeJSONAtomic(m.resultPath(id, page.ID), final); err != nil {
			m.failPage(id, page.ID, err, time.Since(started), false)
			continue
		}
		m.completePage(id, page.ID, final, time.Since(started), allFailed)
	}

	_, _ = m.store.MutateDocument(id, func(current *database.DocumentProject) error {
		if current.Status == database.DocumentCancelled {
			return nil
		}
		now := time.Now().UTC()
		current.Status = database.DocumentCompleted
		current.CompletedAt = &now
		if current.FailedPages > 0 {
			current.Error = fmt.Sprintf("%d 页处理失败,可重新运行失败页面", current.FailedPages)
		} else {
			current.Error = ""
		}
		return nil
	})
}

func (m *documentManager) resolveModels(document database.DocumentProject) ([]model.Resolved, *model.Resolved, error) {
	cfg := m.server.currentConfig()
	engines, err := cfg.ResolveMany(document.Engines)
	if err != nil {
		return nil, nil, err
	}
	if len(engines) == 0 {
		return nil, nil, errors.New("文档没有可用的基础模型")
	}
	var arbiterModel *model.Resolved
	if document.Arbiter != "" {
		resolved, err := cfg.Resolve(document.Arbiter)
		if err != nil {
			return nil, nil, err
		}
		arbiterModel = &resolved
	}
	return engines, arbiterModel, nil
}

func (m *documentManager) failPage(id, pageID string, cause error, duration time.Duration, resultReady bool) {
	_, _ = m.store.MutateDocument(id, func(document *database.DocumentProject) error {
		index := slices.IndexFunc(document.Pages, func(page database.DocumentPage) bool { return page.ID == pageID })
		if index < 0 {
			return errors.New("页面记录不存在")
		}
		page := &document.Pages[index]
		page.Status = database.PageFailed
		page.ResultReady = resultReady
		page.DurationMS = duration.Milliseconds()
		page.Error = cause.Error()
		page.UpdatedAt = time.Now().UTC()
		return nil
	})
}

func (m *documentManager) completePage(id, pageID string, final arbiter.Final, duration time.Duration, allFailed bool) {
	_, _ = m.store.MutateDocument(id, func(document *database.DocumentProject) error {
		index := slices.IndexFunc(document.Pages, func(page database.DocumentPage) bool { return page.ID == pageID })
		if index < 0 {
			return errors.New("页面记录不存在")
		}
		page := &document.Pages[index]
		page.ResultReady = true
		page.Confidence = final.Confidence
		page.Segments = len(final.Segments)
		page.PendingDisputes = pendingDisputeCount(final)
		page.DurationMS = duration.Milliseconds()
		page.Revision++
		page.UpdatedAt = time.Now().UTC()
		if allFailed {
			page.Status = database.PageFailed
			page.Error = "所有引擎均失败,请检查 Provider 连接与模型配置"
		} else {
			page.Status = database.PageCompleted
			page.Error = ""
		}
		return nil
	})
}

func (s *Server) handleDocuments(w http.ResponseWriter, r *http.Request) {
	if _, err := s.getDocumentManager(); err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	auth, _ := authFromRequest(r)
	documents := s.Store.Documents(auth.User.ID, auth.User.Role == database.RoleAdmin)
	for index := range documents {
		documents[index].Pages = nil
	}
	writeJSON(w, http.StatusOK, documents)
}

func (s *Server) handleCreateDocument(w http.ResponseWriter, r *http.Request) {
	manager, err := s.getDocumentManager()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	auth, _ := authFromRequest(r)
	settings, err := s.documentSettingsFromQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.ContentLength > maxDocumentBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "文档不能超过 1 GiB")
		return
	}
	temporary, err := os.CreateTemp(manager.root, ".upload-*.tmp")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建上传文件失败: "+err.Error())
		return
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	_ = temporary.Chmod(0o600)
	r.Body = http.MaxBytesReader(w, r.Body, maxDocumentBytes)
	written, copyErr := io.CopyBuffer(temporary, r.Body, make([]byte, 1<<20))
	if copyErr == nil {
		copyErr = temporary.Sync()
	}
	if copyErr != nil {
		temporary.Close()
		writeErr(w, http.StatusRequestEntityTooLarge, "上传文档失败: "+copyErr.Error())
		return
	}
	if written == 0 {
		temporary.Close()
		writeErr(w, http.StatusBadRequest, "上传文档为空")
		return
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		temporary.Close()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	header := make([]byte, 512)
	headerBytes, _ := io.ReadFull(temporary, header)
	header = header[:headerBytes]
	if err := temporary.Close(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	sourceType := ""
	mimeType := http.DetectContentType(header)
	switch {
	case bytes.HasPrefix(header, []byte("%PDF-")):
		sourceType, mimeType = "pdf", "application/pdf"
	case strings.HasPrefix(mimeType, "image/"):
		sourceType = "image"
		if written > maxDocumentImageBytes {
			writeErr(w, http.StatusRequestEntityTooLarge, "单张图片不能超过 32 MiB")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "仅支持 PDF、PNG、JPEG、WebP 和 GIF")
		return
	}
	name := cleanDocumentName(r.URL.Query().Get("filename"), sourceType)
	document, err := s.Store.CreateDocument(
		auth.User, name, sourceType, mimeType, written,
		settings.Engines, settings.Arbiter, settings.AutoArbitrate,
	)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建文档记录失败: "+err.Error())
		return
	}
	projectDir := manager.projectDir(document.ID)
	if err := os.MkdirAll(filepath.Join(projectDir, "pages"), 0o700); err == nil {
		err = os.MkdirAll(filepath.Join(projectDir, "results"), 0o700)
	}
	if err == nil {
		err = os.Rename(temporaryPath, manager.sourcePath(document))
	}
	if err != nil {
		_, _ = s.Store.DeleteDocument(document.ID)
		_ = os.RemoveAll(projectDir)
		writeErr(w, http.StatusInternalServerError, "保存上传文档失败: "+err.Error())
		return
	}
	if err := manager.enqueue(documentJob{id: document.ID, kind: documentJobPrepare}); err != nil {
		manager.failDocument(document.ID, r.Context(), err)
	}
	writeJSON(w, http.StatusCreated, document)
}

type documentRunSettings struct {
	Engines       []string `json:"engines"`
	Arbiter       string   `json:"arbiter"`
	AutoArbitrate bool     `json:"auto_arbitrate"`
}

func (s *Server) documentSettingsFromQuery(r *http.Request) (documentRunSettings, error) {
	cfg := s.currentConfig()
	engines := pipeline.SplitList(r.URL.Query().Get("engines"))
	if len(engines) == 0 {
		engines = append([]string(nil), cfg.Engines...)
	}
	arbiterRef := cfg.Arbiter
	if r.URL.Query().Has("arbiter") {
		arbiterRef = strings.TrimSpace(r.URL.Query().Get("arbiter"))
	}
	autoArbitrate := !strings.EqualFold(r.URL.Query().Get("auto_arbitrate"), "false")
	settings := documentRunSettings{Engines: engines, Arbiter: arbiterRef, AutoArbitrate: autoArbitrate}
	return settings, s.validateDocumentSettings(settings)
}

func (s *Server) validateDocumentSettings(settings documentRunSettings) error {
	if len(settings.Engines) == 0 {
		return errors.New("请至少选择一个基础模型")
	}
	cfg := s.currentConfig()
	if _, err := cfg.ResolveMany(settings.Engines); err != nil {
		return err
	}
	if settings.Arbiter != "" {
		if _, err := cfg.Resolve(settings.Arbiter); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleDocument(w http.ResponseWriter, r *http.Request) {
	document, ok := s.ownedDocument(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, document)
}

func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	manager, err := s.getDocumentManager()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	document, ok := s.ownedDocument(w, r)
	if !ok {
		return
	}
	manager.cancel(document.ID)
	if _, err := s.Store.DeleteDocument(document.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.RemoveAll(manager.projectDir(document.ID)); err != nil {
		writeErr(w, http.StatusInternalServerError, "项目记录已删除,但文件清理失败: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRunDocument(w http.ResponseWriter, r *http.Request) {
	manager, err := s.getDocumentManager()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	document, ok := s.ownedDocument(w, r)
	if !ok {
		return
	}
	if document.Status == database.DocumentPreparing {
		writeErr(w, http.StatusConflict, "页面仍在服务端准备中")
		return
	}
	if document.Status == database.DocumentProcessing {
		writeJSON(w, http.StatusOK, document)
		return
	}
	var settings documentRunSettings
	if err := decodeJSON(w, r, &settings); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.validateDocumentSettings(settings); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if document.PreparedPages != document.PageCount || document.PageCount == 0 {
		document, err = s.Store.MutateDocument(document.ID, func(next *database.DocumentProject) error {
			next.Engines = append([]string(nil), settings.Engines...)
			next.Arbiter = settings.Arbiter
			next.AutoArbitrate = settings.AutoArbitrate
			next.Status = database.DocumentPreparing
			next.CompletedAt = nil
			next.Error = ""
			return nil
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := manager.enqueue(documentJob{id: document.ID, kind: documentJobPrepare}); err != nil {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, document)
		return
	}
	document, err = s.Store.MutateDocument(document.ID, func(next *database.DocumentProject) error {
		next.Engines = append([]string(nil), settings.Engines...)
		next.Arbiter = settings.Arbiter
		next.AutoArbitrate = settings.AutoArbitrate
		next.Status = database.DocumentReady
		next.CompletedAt = nil
		next.Error = ""
		hasPending := slices.ContainsFunc(next.Pages, func(page database.DocumentPage) bool {
			return page.Status != database.PageCompleted && page.Status != database.PageFailed
		})
		for index := range next.Pages {
			if next.Pages[index].Status == database.PageFailed {
				next.Pages[index].Status = database.PageQueued
				next.Pages[index].ResultReady = false
				next.Pages[index].Error = ""
			} else if !hasPending && next.FailedPages == 0 && next.Pages[index].Status == database.PageCompleted {
				next.Pages[index].Status = database.PageQueued
				next.Pages[index].ResultReady = false
			}
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := manager.enqueue(documentJob{id: document.ID, kind: documentJobProcess}); err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, document)
}

func (s *Server) handleCancelDocument(w http.ResponseWriter, r *http.Request) {
	manager, err := s.getDocumentManager()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	document, ok := s.ownedDocument(w, r)
	if !ok {
		return
	}
	document, err = s.Store.MutateDocument(document.ID, func(next *database.DocumentProject) error {
		next.Status = database.DocumentCancelled
		next.Error = ""
		for index := range next.Pages {
			if next.Pages[index].Status == database.PageProcessing {
				next.Pages[index].Status = database.PageQueued
			}
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	manager.cancel(document.ID)
	writeJSON(w, http.StatusOK, document)
}

func (s *Server) handleDocumentPageOrder(w http.ResponseWriter, r *http.Request) {
	document, ok := s.ownedDocument(w, r)
	if !ok {
		return
	}
	if document.Status == database.DocumentProcessing {
		writeErr(w, http.StatusConflict, "识别过程中不能调整页序")
		return
	}
	var request struct {
		PageIDs []string `json:"page_ids"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(request.PageIDs) != len(document.Pages) {
		writeErr(w, http.StatusBadRequest, "页序必须包含项目中的全部页面")
		return
	}
	document, err := s.Store.MutateDocument(document.ID, func(next *database.DocumentProject) error {
		byID := make(map[string]database.DocumentPage, len(next.Pages))
		for _, page := range next.Pages {
			byID[page.ID] = page
		}
		ordered := make([]database.DocumentPage, len(request.PageIDs))
		for index, id := range request.PageIDs {
			page, exists := byID[id]
			if !exists {
				return errors.New("页序包含未知或重复页面")
			}
			ordered[index] = page
			delete(byID, id)
		}
		if len(byID) != 0 {
			return errors.New("页序包含未知或重复页面")
		}
		next.Pages = ordered
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, document)
}

func (s *Server) handleDeleteDocumentPage(w http.ResponseWriter, r *http.Request) {
	manager, err := s.getDocumentManager()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	document, ok := s.ownedDocument(w, r)
	if !ok {
		return
	}
	if document.Status == database.DocumentPreparing || document.Status == database.DocumentProcessing {
		writeErr(w, http.StatusConflict, "页面准备或识别过程中不能删除页面")
		return
	}
	if len(document.Pages) <= 1 {
		writeErr(w, http.StatusBadRequest, "项目至少需要保留一页")
		return
	}
	page, ok := documentPage(document, r.PathValue("pageID"))
	if !ok {
		writeErr(w, http.StatusNotFound, "页面不存在")
		return
	}
	document, err = s.Store.MutateDocument(document.ID, func(next *database.DocumentProject) error {
		next.Pages = slices.DeleteFunc(next.Pages, func(candidate database.DocumentPage) bool { return candidate.ID == page.ID })
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.Remove(manager.pageImagePath(document, page))
	_ = os.Remove(manager.resultPath(document.ID, page.ID))
	writeJSON(w, http.StatusOK, document)
}

func (s *Server) handleDocumentPageImage(w http.ResponseWriter, r *http.Request) {
	manager, err := s.getDocumentManager()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	document, ok := s.ownedDocument(w, r)
	if !ok {
		return
	}
	page, ok := documentPage(document, r.PathValue("pageID"))
	if !ok {
		writeErr(w, http.StatusNotFound, "页面不存在")
		return
	}
	if !page.ImageReady {
		writeErr(w, http.StatusConflict, "页面图像尚未准备完成")
		return
	}
	file, err := os.Open(manager.pageImagePath(document, page))
	if err != nil {
		writeErr(w, http.StatusNotFound, "页面图像不存在")
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	contentType := "image/jpeg"
	if document.SourceType == "image" {
		contentType = document.MimeType
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, page.ID, stat.ModTime(), file)
}

func (s *Server) handleDocumentPageResult(w http.ResponseWriter, r *http.Request) {
	manager, err := s.getDocumentManager()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	document, ok := s.ownedDocument(w, r)
	if !ok {
		return
	}
	page, ok := documentPage(document, r.PathValue("pageID"))
	if !ok || !page.ResultReady {
		writeErr(w, http.StatusNotFound, "页面结果尚不存在")
		return
	}
	final, err := readFinal(manager.resultPath(document.ID, page.ID))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取页面结果失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, final)
}

func (s *Server) handleUpdateDocumentPageResult(w http.ResponseWriter, r *http.Request) {
	manager, err := s.getDocumentManager()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	document, ok := s.ownedDocument(w, r)
	if !ok {
		return
	}
	page, ok := documentPage(document, r.PathValue("pageID"))
	if !ok {
		writeErr(w, http.StatusNotFound, "页面不存在")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxResultBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var final arbiter.Final
	if err := decoder.Decode(&final); err != nil {
		writeErr(w, http.StatusBadRequest, "页面结果 JSON 无效: "+err.Error())
		return
	}
	if err := writeJSONAtomic(manager.resultPath(document.ID, page.ID), final); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存页面结果失败: "+err.Error())
		return
	}
	document, err = s.Store.MutateDocument(document.ID, func(next *database.DocumentProject) error {
		index := slices.IndexFunc(next.Pages, func(candidate database.DocumentPage) bool { return candidate.ID == page.ID })
		if index < 0 {
			return errors.New("页面不存在")
		}
		nextPage := &next.Pages[index]
		nextPage.ResultReady = true
		nextPage.Status = database.PageCompleted
		nextPage.Confidence = final.Confidence
		nextPage.Segments = len(final.Segments)
		nextPage.PendingDisputes = pendingDisputeCount(final)
		nextPage.Revision++
		nextPage.Error = ""
		nextPage.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, document)
}

type documentDisputeItem struct {
	PageID       string               `json:"page_id"`
	PageNumber   int                  `json:"page_number"`
	SourcePage   int                  `json:"source_page"`
	SegmentIndex int                  `json:"segment_index"`
	Segment      arbiter.FinalSegment `json:"segment"`
}

func (s *Server) handleDocumentDisputes(w http.ResponseWriter, r *http.Request) {
	manager, err := s.getDocumentManager()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	document, ok := s.ownedDocument(w, r)
	if !ok {
		return
	}
	items := make([]documentDisputeItem, 0, document.PendingDisputes)
	for _, page := range document.Pages {
		if !page.ResultReady || page.PendingDisputes == 0 {
			continue
		}
		final, err := readFinal(manager.resultPath(document.ID, page.ID))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取审计结果失败: "+err.Error())
			return
		}
		for segmentIndex, segment := range final.Segments {
			if segment.Disputed && segment.Source == arbiter.SourceFallback {
				items = append(items, documentDisputeItem{
					PageID: page.ID, PageNumber: page.PageNumber, SourcePage: page.SourcePage,
					SegmentIndex: segmentIndex, Segment: segment,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleDocumentTextExport(w http.ResponseWriter, r *http.Request) {
	manager, err := s.getDocumentManager()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	document, ok := s.ownedDocument(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", attachmentName(documentStem(document.Name)+".txt"))
	first := true
	for _, page := range document.Pages {
		if !page.ResultReady {
			continue
		}
		final, err := readFinal(manager.resultPath(document.ID, page.ID))
		if err != nil {
			continue
		}
		if !first {
			_, _ = io.WriteString(w, "\n\n\f\n\n")
		}
		first = false
		_, _ = fmt.Fprintf(w, "第 %d 页\n%s", page.PageNumber, strings.TrimSpace(final.Text))
	}
}

func (s *Server) handleDocumentAuditExport(w http.ResponseWriter, r *http.Request) {
	manager, err := s.getDocumentManager()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	document, ok := s.ownedDocument(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", attachmentName(documentStem(document.Name)+"-audit.json"))
	metadata := struct {
		ID         string    `json:"id"`
		Name       string    `json:"name"`
		SourceType string    `json:"source_type"`
		SizeBytes  int64     `json:"size_bytes"`
		CreatedAt  time.Time `json:"created_at"`
		ExportedAt time.Time `json:"exported_at"`
	}{document.ID, document.Name, document.SourceType, document.SizeBytes, document.CreatedAt, time.Now().UTC()}
	metadataJSON, _ := json.Marshal(metadata)
	_, _ = io.WriteString(w, "{\"schema\":\"betterocr.document-audit.v1\",\"document\":")
	_, _ = w.Write(metadataJSON)
	_, _ = io.WriteString(w, ",\"pages\":[")
	for index, page := range document.Pages {
		if index > 0 {
			_, _ = io.WriteString(w, ",")
		}
		var result *arbiter.Final
		if page.ResultReady {
			if final, err := readFinal(manager.resultPath(document.ID, page.ID)); err == nil {
				result = &final
			}
		}
		entry := struct {
			PageNumber int            `json:"page_number"`
			SourcePage int            `json:"source_page"`
			Status     string         `json:"status"`
			DurationMS int64          `json:"duration_ms,omitempty"`
			Error      string         `json:"error,omitempty"`
			Result     *arbiter.Final `json:"result,omitempty"`
		}{page.PageNumber, page.SourcePage, page.Status, page.DurationMS, page.Error, result}
		encoded, _ := json.Marshal(entry)
		_, _ = w.Write(encoded)
	}
	_, _ = io.WriteString(w, "]}\n")
}

func (s *Server) ownedDocument(w http.ResponseWriter, r *http.Request) (database.DocumentProject, bool) {
	if _, err := s.getDocumentManager(); err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return database.DocumentProject{}, false
	}
	document, exists := s.Store.Document(r.PathValue("id"))
	if !exists {
		writeErr(w, http.StatusNotFound, "文档项目不存在")
		return database.DocumentProject{}, false
	}
	auth, ok := authFromRequest(r)
	if !ok || auth.User.ID != document.UserID && auth.User.Role != database.RoleAdmin {
		writeErr(w, http.StatusForbidden, "无权访问该文档项目")
		return database.DocumentProject{}, false
	}
	return document, true
}

func documentPage(document database.DocumentProject, pageID string) (database.DocumentPage, bool) {
	index := slices.IndexFunc(document.Pages, func(page database.DocumentPage) bool { return page.ID == pageID })
	if index < 0 {
		return database.DocumentPage{}, false
	}
	return document.Pages[index], true
}

func pendingDisputeCount(final arbiter.Final) int {
	count := 0
	for _, segment := range final.Segments {
		if segment.Disputed && segment.Source == arbiter.SourceFallback {
			count++
		}
	}
	return count
}

func readFinal(path string) (arbiter.Final, error) {
	file, err := os.Open(path)
	if err != nil {
		return arbiter.Final{}, err
	}
	defer file.Close()
	var final arbiter.Final
	err = json.NewDecoder(io.LimitReader(file, maxResultBytes+1)).Decode(&final)
	return final, err
}

func writeJSONAtomic(path string, value any) error {
	return writeAtomic(path, 0o600, func(writer io.Writer) error {
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	})
}

func writeAtomic(path string, mode os.FileMode, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".write-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if err := write(temporary); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func linkOrCopy(source, target string) error {
	if err := os.Link(source, target); err == nil {
		return nil
	}
	return writeAtomic(target, 0o600, func(writer io.Writer) error {
		file, err := os.Open(source)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.CopyBuffer(writer, file, make([]byte, 1<<20))
		return err
	})
}

func cleanDocumentName(name, sourceType string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	if len(name) > 240 {
		name = name[:240]
	}
	if name == "" {
		if sourceType == "pdf" {
			return "未命名.pdf"
		}
		return "未命名图片"
	}
	return name
}

func documentStem(name string) string {
	if dot := strings.LastIndex(name, "."); dot > 0 {
		name = name[:dot]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "betterocr-document"
	}
	return name
}

func attachmentName(filename string) string {
	return mime.FormatMediaType("attachment", map[string]string{"filename": filename})
}
