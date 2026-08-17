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
	"time"

	"github.com/lieyanc/BetterOCR/internal/pipeline"
	"github.com/lieyanc/BetterOCR/web"
)

// maxImageBytes 限制上传体积,防止误传大文件占满内存。
const maxImageBytes = 32 << 20

// Server 是 Web 模式的配置。零值可用,所有字段均可选。
type Server struct {
	// DefaultEngines / DefaultArbiter / BaseURL 是页面预填的默认值,
	// 每次请求可逐项覆盖。
	DefaultEngines []string
	DefaultArbiter string
	BaseURL        string
	// APIKey 是服务端默认密钥;只告知页面"是否已配置",绝不下发明文。
	APIKey string
	// Timeout 是单次识别的端到端超时,零值取 2 分钟。
	Timeout time.Duration
	// HTTPClient 为 nil 时使用 http.DefaultClient。
	HTTPClient *http.Client
}

// Handler 返回完整的 HTTP 处理器:/api/* 为接口,其余为内嵌前端。
func (s *Server) Handler() http.Handler {
	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		panic(err) // embed 保证 dist 目录存在
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("POST /api/ocr", s.handleOCR)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "未知接口: "+r.URL.Path)
	})
	// 静态路由不能写成 "GET /":它与 "/api/" 在方法与路径上互不包含,
	// ServeMux 会判定为歧义模式而 panic;方法检查移到 handler 内部。
	mux.Handle("/", staticHandler(dist))
	return mux
}

func (s *Server) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 2 * time.Minute
}

// configResponse 是 GET /api/config 的响应,用于页面预填表单。
type configResponse struct {
	Engines   []string `json:"engines"`
	Arbiter   string   `json:"arbiter"`
	BaseURL   string   `json:"base_url"`
	HasAPIKey bool     `json:"has_api_key"`
	TimeoutMS int64    `json:"timeout_ms"`
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	engines := s.DefaultEngines
	if engines == nil {
		engines = []string{}
	}
	writeJSON(w, http.StatusOK, configResponse{
		Engines:   engines,
		Arbiter:   s.DefaultArbiter,
		BaseURL:   s.BaseURL,
		HasAPIKey: s.APIKey != "",
		TimeoutMS: s.timeout().Milliseconds(),
	})
}

// handleOCR 接收 multipart 表单(image 文件 + engines/arbiter/base_url/api_key
// 字段),跑一遍与 CLI 完全相同的流水线,返回 arbiter.Final JSON。
func (s *Server) handleOCR(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImageBytes)
	if err := r.ParseMultipartForm(maxImageBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "解析上传表单失败: "+err.Error())
		return
	}
	file, _, err := r.FormFile("image")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "缺少图片文件字段 image")
		return
	}
	defer file.Close()
	image, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取图片失败: "+err.Error())
		return
	}
	if ct := http.DetectContentType(image); !strings.HasPrefix(ct, "image/") {
		writeErr(w, http.StatusBadRequest, "上传内容不是可识别的图片格式: "+ct)
		return
	}

	engines := pipeline.SplitList(formOr(r, "engines", strings.Join(s.DefaultEngines, ",")))
	if len(engines) == 0 {
		writeErr(w, http.StatusBadRequest, "未指定引擎模型(engines)")
		return
	}
	cfg := pipeline.Config{
		Engines:    engines,
		Arbiter:    formOr(r, "arbiter", s.DefaultArbiter),
		BaseURL:    formOr(r, "base_url", s.BaseURL),
		APIKey:     formOr(r, "api_key", s.APIKey),
		HTTPClient: s.HTTPClient,
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.timeout())
	defer cancel()
	final, err := pipeline.Run(ctx, cfg, image)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, final)
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
