package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/lieyanc/BetterOCR/internal/arbiter"
)

// testPNG 在测试内生成一张最小合法 PNG,避免外部测试资产。
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.White)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeUpstream 模拟 OpenAI 兼容端点,记录每次请求的 Authorization 头。
func fakeUpstream(content string) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		body, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
		_, _ = w.Write(body)
	}))
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), auths...)
	}
}

// multipartBody 构造 /api/ocr 的请求体。image 为 nil 时不带图片字段。
func multipartBody(t *testing.T, image []byte, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if image != nil {
		fw, err := mw.CreateFormFile("image", "test.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(image); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func postOCR(t *testing.T, h http.Handler, image []byte, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartBody(t, image, fields)
	req := httptest.NewRequest(http.MethodPost, "/api/ocr", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestOCREndToEnd 走完整链路:multipart 上传 → 双引擎并发 → 共识融合 → JSON。
func TestOCREndToEnd(t *testing.T) {
	upstream, auths := fakeUpstream(`[{"text":"hello world","confidence":0.9},{"text":"second line","confidence":0.8}]`)
	defer upstream.Close()

	srv := &Server{APIKey: "server-key", Timeout: 30 * time.Second}
	rec := postOCR(t, srv.Handler(), testPNG(t), map[string]string{
		"engines":  "tiny-a,tiny-b",
		"base_url": upstream.URL,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}

	var final arbiter.Final
	if err := json.Unmarshal(rec.Body.Bytes(), &final); err != nil {
		t.Fatal(err)
	}
	if final.Text != "hello world\nsecond line" {
		t.Errorf("text = %q", final.Text)
	}
	// 两引擎一致:每行 1-(0.1*0.1)=0.99 与 1-(0.2*0.2)=0.96,均值 0.975
	if final.Confidence != 0.975 {
		t.Errorf("confidence = %v, want 0.975", final.Confidence)
	}
	if s := final.Stats; s.Engines != 2 || s.ConsensusRows != 2 || s.FailedEngines != 0 {
		t.Errorf("stats = %+v", s)
	}
	for _, l := range final.Lines {
		if l.Source != arbiter.SourceConsensus {
			t.Errorf("line %+v, want consensus", l)
		}
	}
	// 未在请求里带 api_key,应回落到服务端默认密钥
	for _, a := range auths() {
		if a != "Bearer server-key" {
			t.Errorf("Authorization = %q, want server key", a)
		}
	}
}

// TestOCRRequestOverridesKey 验证请求内的 api_key 覆盖服务端默认值。
func TestOCRRequestOverridesKey(t *testing.T) {
	upstream, auths := fakeUpstream(`[{"text":"x","confidence":1}]`)
	defer upstream.Close()

	srv := &Server{APIKey: "server-key"}
	rec := postOCR(t, srv.Handler(), testPNG(t), map[string]string{
		"engines":  "tiny",
		"base_url": upstream.URL,
		"api_key":  "user-key",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	got := auths()
	if len(got) != 1 || got[0] != "Bearer user-key" {
		t.Errorf("auths = %v, want user key", got)
	}
}

// TestOCRBadRequests 覆盖各类 400:缺图、缺引擎、非图片内容。
func TestOCRBadRequests(t *testing.T) {
	h := (&Server{}).Handler()

	cases := []struct {
		name    string
		image   []byte
		fields  map[string]string
		wantMsg string
	}{
		{name: "缺少图片", image: nil, fields: map[string]string{"engines": "m"}, wantMsg: "image"},
		{name: "缺少引擎", image: testPNG(t), fields: nil, wantMsg: "引擎"},
		{name: "非图片内容", image: []byte("plain text"), fields: map[string]string{"engines": "m"}, wantMsg: "图片格式"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postOCR(t, h, c.image, c.fields)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
			}
			var e struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || !strings.Contains(e.Error, c.wantMsg) {
				t.Errorf("error = %q, want contains %q", e.Error, c.wantMsg)
			}
		})
	}
}

// TestConfigEndpoint 验证页面预填数据,且不泄露密钥明文。
func TestConfigEndpoint(t *testing.T) {
	srv := &Server{
		DefaultEngines: []string{"m1", "m2"},
		DefaultArbiter: "big",
		BaseURL:        "http://example/v1",
		APIKey:         "secret",
	}
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatal("config response leaks the API key")
	}
	var cfg configResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Engines) != 2 || cfg.Arbiter != "big" || cfg.BaseURL != "http://example/v1" || !cfg.HasAPIKey {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.TimeoutMS != (2 * time.Minute).Milliseconds() {
		t.Errorf("timeout_ms = %d, want default 2m", cfg.TimeoutMS)
	}
}

// TestStaticHandler 用内存 FS 验证静态服务:直出、SPA 回退、资产缓存头。
func TestStaticHandler(t *testing.T) {
	dist := fstest.MapFS{
		"index.html":       &fstest.MapFile{Data: []byte("<html>app</html>")},
		"assets/x-h4sh.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	h := staticHandler(dist)

	get := func(p string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		return rec
	}

	if rec := get("/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "app") {
		t.Errorf("GET / = %d %q", rec.Code, rec.Body)
	}
	if rec := get("/assets/x-h4sh.js"); rec.Code != 200 ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("GET asset = %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	// SPA 回退:未知路径也返回 index.html
	if rec := get("/some/client/route"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "app") {
		t.Errorf("SPA fallback = %d %q", rec.Code, rec.Body)
	}
}

// TestStaticHandlerUnbuilt 验证前端未构建时返回可读的指引而非 404。
func TestStaticHandlerUnbuilt(t *testing.T) {
	h := staticHandler(fstest.MapFS{".gitkeep": &fstest.MapFile{}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "npm") {
		t.Errorf("unbuilt = %d %q", rec.Code, rec.Body)
	}
}
