// Package server 提供 Web 模式:内嵌前端静态页 + OCR HTTP API。
package server

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/lieyanc/BetterOCR/internal/agents"
	"github.com/lieyanc/BetterOCR/internal/arbiter"
	"github.com/lieyanc/BetterOCR/internal/config"
	"github.com/lieyanc/BetterOCR/internal/database"
	"github.com/lieyanc/BetterOCR/internal/documents"
	"github.com/lieyanc/BetterOCR/internal/model"
	"github.com/lieyanc/BetterOCR/internal/pipeline"
	"github.com/lieyanc/BetterOCR/web"
)

// maxImageBytes 限制上传体积,防止误传大文件占满内存。
const maxImageBytes = 32 << 20

// Server is the web application configuration.
type Server struct {
	// Config contains the server-side provider catalog and credentials.
	Config config.Config
	// ConfigPath is the single configuration file edited by the admin API.
	ConfigPath string
	// Timeout is a legacy test override applied to both stages when set.
	Timeout time.Duration
	// EngineTimeout and ArbiterTimeout override their respective stage in tests.
	EngineTimeout  time.Duration
	ArbiterTimeout time.Duration
	// HTTPClient 为 nil 时使用 http.DefaultClient。
	HTTPClient *http.Client
	// Store enables login, authorization and task history.
	// A nil store keeps the handler useful for isolated package tests.
	Store *database.Store
	// DocumentRoot overrides the default directory beside database.json.
	DocumentRoot string
	// PDFRenderer is injectable for tests. Production uses embedded PDFium WASM.
	PDFRenderer documents.PageRenderer

	configMu        sync.RWMutex
	documentOnce    sync.Once
	documentManager *documentManager
	documentInitErr error
}

// Handler 返回完整的 HTTP 处理器:/api/* 为接口,其余为内嵌前端。
func (s *Server) Handler() http.Handler {
	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		panic(err) // embed 保证 dist 目录存在
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/setup/status", s.handleSetupStatus)
	mux.HandleFunc("POST /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.Handle("GET /api/auth/session", s.requireAuth(http.HandlerFunc(s.handleSession)))
	mux.Handle("POST /api/auth/logout", s.requireAuth(http.HandlerFunc(s.handleLogout)))
	mux.Handle("GET /api/config", s.requireAuth(http.HandlerFunc(s.handleConfig)))
	mux.Handle("POST /api/ocr", s.requireAuth(http.HandlerFunc(s.handleOCR)))
	mux.Handle("POST /api/ocr/stream", s.requireAuth(http.HandlerFunc(s.handleOCRStream)))
	mux.Handle("POST /api/arbitrate/stream", s.requireAuth(http.HandlerFunc(s.handleArbitrateStream)))
	mux.Handle("GET /api/tasks", s.requireAuth(http.HandlerFunc(s.handleTasks)))
	mux.Handle("GET /api/documents", s.requireAuth(http.HandlerFunc(s.handleDocuments)))
	mux.Handle("POST /api/documents", s.requireAuth(http.HandlerFunc(s.handleCreateDocument)))
	mux.Handle("GET /api/documents/{id}", s.requireAuth(http.HandlerFunc(s.handleDocument)))
	mux.Handle("GET /api/documents/{id}/events", s.requireAuth(http.HandlerFunc(s.handleDocumentProgress)))
	mux.Handle("DELETE /api/documents/{id}", s.requireAuth(http.HandlerFunc(s.handleDeleteDocument)))
	mux.Handle("POST /api/documents/{id}/run", s.requireAuth(http.HandlerFunc(s.handleRunDocument)))
	mux.Handle("POST /api/documents/{id}/cancel", s.requireAuth(http.HandlerFunc(s.handleCancelDocument)))
	mux.Handle("PUT /api/documents/{id}/pages/order", s.requireAuth(http.HandlerFunc(s.handleDocumentPageOrder)))
	mux.Handle("GET /api/documents/{id}/pages/{pageID}/image", s.requireAuth(http.HandlerFunc(s.handleDocumentPageImage)))
	mux.Handle("GET /api/documents/{id}/pages/{pageID}/result", s.requireAuth(http.HandlerFunc(s.handleDocumentPageResult)))
	mux.Handle("PUT /api/documents/{id}/pages/{pageID}/result", s.requireAuth(http.HandlerFunc(s.handleUpdateDocumentPageResult)))
	mux.Handle("DELETE /api/documents/{id}/pages/{pageID}", s.requireAuth(http.HandlerFunc(s.handleDeleteDocumentPage)))
	mux.Handle("GET /api/documents/{id}/disputes", s.requireAuth(http.HandlerFunc(s.handleDocumentDisputes)))
	mux.Handle("GET /api/documents/{id}/export/text", s.requireAuth(http.HandlerFunc(s.handleDocumentTextExport)))
	mux.Handle("GET /api/documents/{id}/export/audit", s.requireAuth(http.HandlerFunc(s.handleDocumentAuditExport)))
	mux.Handle("GET /api/admin/users", s.requireAdmin(http.HandlerFunc(s.handleUsers)))
	mux.Handle("POST /api/admin/users", s.requireAdmin(http.HandlerFunc(s.handleCreateUser)))
	mux.Handle("PUT /api/admin/users/{id}", s.requireAdmin(http.HandlerFunc(s.handleUpdateUser)))
	mux.Handle("DELETE /api/admin/users/{id}", s.requireAdmin(http.HandlerFunc(s.handleDeleteUser)))
	mux.Handle("GET /api/admin/settings", s.requireAdmin(http.HandlerFunc(s.handleAdminSettings)))
	mux.Handle("PUT /api/admin/settings", s.requireAdmin(http.HandlerFunc(s.handleUpdateAdminSettings)))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "未知接口: "+r.URL.Path)
	})
	// 静态路由不能写成 "GET /":它与 "/api/" 在方法与路径上互不包含,
	// ServeMux 会判定为歧义模式而 panic;方法检查移到 handler 内部。
	mux.Handle("/", staticHandler(dist))
	return mux
}

func (s *Server) engineTimeout() time.Duration {
	if s.EngineTimeout > 0 {
		return s.EngineTimeout
	}
	if s.Timeout > 0 {
		return s.Timeout
	}
	return s.currentConfig().EngineTimeout()
}

func (s *Server) arbiterTimeout() time.Duration {
	if s.ArbiterTimeout > 0 {
		return s.ArbiterTimeout
	}
	if s.Timeout > 0 {
		return s.Timeout
	}
	return s.currentConfig().ArbiterTimeout()
}

func (s *Server) currentConfig() config.Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	cloned := s.Config
	cloned.Engines = append([]string(nil), s.Config.Engines...)
	cloned.Providers = append([]model.Provider(nil), s.Config.Providers...)
	for i := range cloned.Providers {
		cloned.Providers[i].Models = append([]model.Definition(nil), s.Config.Providers[i].Models...)
	}
	return cloned
}

// configResponse exposes selectable metadata but never provider API keys.
type configResponse struct {
	Providers        []providerResponse `json:"providers"`
	Engines          []string           `json:"engines"`
	Arbiter          string             `json:"arbiter"`
	TimeoutMS        int64              `json:"timeout_ms"`
	EngineTimeoutMS  int64              `json:"engine_timeout_ms"`
	ArbiterTimeoutMS int64              `json:"arbiter_timeout_ms"`
	EngineAttempts   int                `json:"engine_max_attempts"`
	ArbiterAttempts  int                `json:"arbiter_max_attempts"`
}

type providerResponse struct {
	ID        string             `json:"id"`
	Alias     string             `json:"alias"`
	BaseURL   string             `json:"base_url"`
	HasAPIKey bool               `json:"has_api_key"`
	Models    []model.Definition `json:"models"`
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	cfg := s.currentConfig()
	engines := cfg.Engines
	if engines == nil {
		engines = []string{}
	}
	providers := make([]providerResponse, 0, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		models := append([]model.Definition(nil), provider.Models...)
		providers = append(providers, providerResponse{
			ID: provider.ID, Alias: provider.DisplayName(), BaseURL: provider.BaseURL,
			HasAPIKey: provider.APIKey != "", Models: models,
		})
	}
	writeJSON(w, http.StatusOK, configResponse{
		Providers: providers,
		Engines:   engines,
		Arbiter:   cfg.Arbiter,
		// timeout_ms remains as a compatibility alias for engine_timeout_ms.
		TimeoutMS:        s.engineTimeout().Milliseconds(),
		EngineTimeoutMS:  s.engineTimeout().Milliseconds(),
		ArbiterTimeoutMS: s.arbiterTimeout().Milliseconds(),
		EngineAttempts:   cfg.EngineAttempts(),
		ArbiterAttempts:  cfg.ArbiterAttempts(),
	})
}

// handleOCR accepts an image and configured model references. Connections and
// credentials are resolved exclusively from the server-side configuration.
func (s *Server) handleOCR(w http.ResponseWriter, r *http.Request) {
	image, runConfig, ok := s.parseOCRRequest(w, r)
	if !ok {
		return
	}

	started := time.Now()
	taskID, ok := s.startTask(w, r, runConfig)
	if !ok {
		return
	}
	if taskID != "" {
		w.Header().Set("X-BetterOCR-Task-ID", taskID)
	}
	final, err := pipeline.Run(r.Context(), runConfig, image)
	if err != nil {
		s.finishTask(taskID, nil, err.Error(), time.Since(started))
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.finishTask(taskID, &final, "", time.Since(started))
	writeJSON(w, http.StatusOK, final)
}

type streamEvent struct {
	Type        string               `json:"type"`
	TaskID      string               `json:"task_id,omitempty"`
	Stage       string               `json:"stage,omitempty"`
	Agent       string               `json:"agent,omitempty"`
	Kind        string               `json:"kind,omitempty"`
	Text        string               `json:"text,omitempty"`
	Attempt     int                  `json:"attempt,omitempty"`
	MaxAttempts int                  `json:"max_attempts,omitempty"`
	Result      *arbiter.Final       `json:"result,omitempty"`
	Resolutions []arbiter.Resolution `json:"resolutions,omitempty"`
	Error       string               `json:"error,omitempty"`
}

// handleOCRStream emits newline-delimited JSON fragments as each upstream
// model generates text, followed by one result event with the fused output.
func (s *Server) handleOCRStream(w http.ResponseWriter, r *http.Request) {
	image, runConfig, ok := s.parseOCRRequest(w, r)
	if !ok {
		return
	}

	started := time.Now()
	taskID, ok := s.startTask(w, r, runConfig)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	controller := http.NewResponseController(w)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var eventMu sync.Mutex
	streamOK := true
	writeEvent := func(event streamEvent) {
		eventMu.Lock()
		defer eventMu.Unlock()
		if !streamOK {
			return
		}
		if err := encoder.Encode(event); err != nil {
			streamOK = false
			cancel()
			return
		}
		if err := controller.Flush(); err != nil {
			streamOK = false
			cancel()
			return
		}
		if event.Type == "result" || event.Type == "error" {
			streamOK = false
		}
	}

	writeEvent(streamEvent{Type: "start", TaskID: taskID})
	runConfig.OnEvent = func(event pipeline.Event) {
		switch event.Type {
		case pipeline.EventDelta, pipeline.EventAttemptStart, pipeline.EventAttemptFailed:
			writeEvent(streamEvent{
				Type: event.Type, Stage: event.Stage, Agent: event.Agent,
				Kind: event.Kind, Text: event.Text, Error: event.Error,
				Attempt: event.Attempt, MaxAttempts: event.MaxAttempts,
			})
		}
	}
	final, err := pipeline.Run(ctx, runConfig, image)
	if err != nil {
		s.finishTask(taskID, nil, err.Error(), time.Since(started))
		writeEvent(streamEvent{Type: "error", Error: err.Error()})
		return
	}
	s.finishTask(taskID, &final, "", time.Since(started))
	writeEvent(streamEvent{Type: "result", TaskID: taskID, Result: &final})
}

// handleArbitrateStream only resolves the submitted disputed segments. It does
// not rerun the base OCR engines, so a user can merge candidates and arbitrate
// the remaining uncertainty independently.
func (s *Server) handleArbitrateStream(w http.ResponseWriter, r *http.Request) {
	image, resolvedModel, disputes, ok := s.parseArbitrationRequest(w, r)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	controller := http.NewResponseController(w)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	streamOK := true
	writeEvent := func(event streamEvent) {
		if !streamOK {
			return
		}
		if err := encoder.Encode(event); err != nil {
			streamOK = false
			cancel()
			return
		}
		if err := controller.Flush(); err != nil {
			streamOK = false
			cancel()
			return
		}
		if event.Type == "result" || event.Type == "error" {
			streamOK = false
		}
	}

	escalator := agents.NewVisionEscalator(resolvedModel, s.HTTPClient)
	currentAttempt := 0
	maxAttempts := s.currentConfig().ArbiterAttempts()
	escalator.OnDelta = func(delta agents.StreamDelta) {
		writeEvent(streamEvent{
			Type: "delta", Stage: pipeline.StageArbiter, Agent: escalator.Name(),
			Kind: string(delta.Kind), Text: delta.Text,
			Attempt: currentAttempt, MaxAttempts: maxAttempts,
		})
	}
	writeEvent(streamEvent{Type: "start"})
	resolutions, err := pipeline.ResolveWithRetry(
		ctx, escalator, image, disputes, s.arbiterTimeout(), maxAttempts,
		func(event pipeline.Event) {
			if event.Type != pipeline.EventAttemptStart && event.Type != pipeline.EventAttemptFailed {
				return
			}
			if event.Type == pipeline.EventAttemptStart {
				currentAttempt = event.Attempt
			}
			writeEvent(streamEvent{
				Type: event.Type, Stage: event.Stage, Agent: event.Agent, Error: event.Error,
				Attempt: event.Attempt, MaxAttempts: event.MaxAttempts,
			})
		},
	)
	if err != nil {
		writeEvent(streamEvent{Type: "error", Error: err.Error()})
		return
	}
	disputeBySegment := make(map[int]arbiter.Dispute, len(disputes))
	for _, dispute := range disputes {
		disputeBySegment[dispute.Segment] = dispute
	}
	filtered := make([]arbiter.Resolution, 0, len(resolutions))
	for _, resolution := range resolutions {
		dispute, exists := disputeBySegment[resolution.Segment]
		if !exists {
			continue
		}
		resolution.Confidence = arbiter.ResolutionConfidence(dispute.Candidates, resolution.Text)
		resolution.From = []string{escalator.Name()}
		filtered = append(filtered, resolution)
	}
	writeEvent(streamEvent{Type: "result", Resolutions: filtered})
}

func (s *Server) parseArbitrationRequest(w http.ResponseWriter, r *http.Request) ([]byte, model.Resolved, []arbiter.Dispute, bool) {
	cfg := s.currentConfig()
	r.Body = http.MaxBytesReader(w, r.Body, maxImageBytes)
	if err := r.ParseMultipartForm(maxImageBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "解析仲裁表单失败: "+err.Error())
		return nil, model.Resolved{}, nil, false
	}
	file, _, err := r.FormFile("image")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "缺少图片文件字段 image")
		return nil, model.Resolved{}, nil, false
	}
	defer file.Close()
	image, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取图片失败: "+err.Error())
		return nil, model.Resolved{}, nil, false
	}
	if ct := http.DetectContentType(image); !strings.HasPrefix(ct, "image/") {
		writeErr(w, http.StatusBadRequest, "上传内容不是可识别的图片格式: "+ct)
		return nil, model.Resolved{}, nil, false
	}
	arbiterRef := formOr(r, "arbiter", cfg.Arbiter)
	if arbiterRef == "" {
		writeErr(w, http.StatusBadRequest, "未选择仲裁模型")
		return nil, model.Resolved{}, nil, false
	}
	resolved, err := cfg.Resolve(arbiterRef)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return nil, model.Resolved{}, nil, false
	}
	var disputes []arbiter.Dispute
	if err := json.Unmarshal([]byte(formOr(r, "disputes", "")), &disputes); err != nil {
		writeErr(w, http.StatusBadRequest, "争议句段格式无效: "+err.Error())
		return nil, model.Resolved{}, nil, false
	}
	if len(disputes) == 0 || len(disputes) > 200 {
		writeErr(w, http.StatusBadRequest, "争议句段数量必须在 1 到 200 之间")
		return nil, model.Resolved{}, nil, false
	}
	for _, dispute := range disputes {
		if dispute.Segment < 0 || len(dispute.Candidates) == 0 || len(dispute.Candidates) > 32 {
			writeErr(w, http.StatusBadRequest, "争议句段缺少有效编号或候选")
			return nil, model.Resolved{}, nil, false
		}
	}
	return image, resolved, disputes, true
}

func (s *Server) parseOCRRequest(w http.ResponseWriter, r *http.Request) ([]byte, pipeline.Config, bool) {
	cfg := s.currentConfig()
	r.Body = http.MaxBytesReader(w, r.Body, maxImageBytes)
	if err := r.ParseMultipartForm(maxImageBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "解析上传表单失败: "+err.Error())
		return nil, pipeline.Config{}, false
	}
	file, _, err := r.FormFile("image")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "缺少图片文件字段 image")
		return nil, pipeline.Config{}, false
	}
	defer file.Close()
	image, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取图片失败: "+err.Error())
		return nil, pipeline.Config{}, false
	}
	if ct := http.DetectContentType(image); !strings.HasPrefix(ct, "image/") {
		writeErr(w, http.StatusBadRequest, "上传内容不是可识别的图片格式: "+ct)
		return nil, pipeline.Config{}, false
	}

	engineRefs := pipeline.SplitList(formOr(r, "engines", strings.Join(cfg.Engines, ",")))
	if len(engineRefs) == 0 {
		writeErr(w, http.StatusBadRequest, "未指定引擎模型(engines)")
		return nil, pipeline.Config{}, false
	}
	engines, err := cfg.ResolveMany(engineRefs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return nil, pipeline.Config{}, false
	}
	var arbiterModel *model.Resolved
	if arbiterRef := formOr(r, "arbiter", cfg.Arbiter); arbiterRef != "" {
		resolved, err := cfg.Resolve(arbiterRef)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return nil, pipeline.Config{}, false
		}
		arbiterModel = &resolved
	}
	return image, pipeline.Config{
		Engines:            engines,
		Arbiter:            arbiterModel,
		DeferArbitration:   strings.EqualFold(formOr(r, "auto_arbitrate", "true"), "false"),
		HTTPClient:         s.HTTPClient,
		EngineTimeout:      s.engineTimeout(),
		ArbiterTimeout:     s.arbiterTimeout(),
		EngineMaxAttempts:  cfg.EngineAttempts(),
		ArbiterMaxAttempts: cfg.ArbiterAttempts(),
	}, true
}

// formOr 返回表单字段值;字段未出现时用默认值。
// 显式提交的空字段视为"清空覆盖"(如清掉仲裁模型),而非回退默认。
func formOr(r *http.Request, key, def string) string {
	if vs, ok := r.MultipartForm.Value[key]; ok && len(vs) > 0 {
		return strings.TrimSpace(vs[0])
	}
	return def
}

// staticHandler 服务内嵌前端:命中的文件直接返回,其余路径回退到
// index.html(SPA);前端尚未构建时返回构建指引。
func staticHandler(dist fs.FS) http.Handler {
	fileServer := http.FileServerFS(dist)
	indexHTML, indexErr := fs.ReadFile(dist, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "." {
			if _, err := fs.Stat(dist, p); err == nil {
				if strings.HasPrefix(p, "assets/") {
					// Vite 产物文件名带内容哈希,可永久缓存
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		if indexErr != nil {
			http.Error(w, "前端尚未构建:请先 cd web && npm install && npm run build,再重新 go build",
				http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(indexHTML)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
