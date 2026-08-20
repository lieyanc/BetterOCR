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

	"github.com/lieyanc/BetterOCR/internal/agents"
	"github.com/lieyanc/BetterOCR/internal/arbiter"
	"github.com/lieyanc/BetterOCR/internal/config"
	"github.com/lieyanc/BetterOCR/internal/model"
	"github.com/lieyanc/BetterOCR/internal/pipeline"
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

func serverConfig(baseURL, apiKey string) config.Config {
	return config.Config{
		Providers: []model.Provider{{
			ID: "test", BaseURL: baseURL, APIKey: apiKey,
			Models: []model.Definition{
				{ID: "tiny-a", Context: 32768, Alias: "Tiny A", API: model.APIOpenAIChatCompletions},
				{ID: "tiny-b", Context: 32768, Alias: "Tiny B", API: model.APIOpenAIChatCompletions},
				{ID: "big", Context: 128000, Alias: "Big", API: model.APIOpenAIChatCompletions},
			},
		}},
		Engines: []string{"test/tiny-a", "test/tiny-b"},
		Arbiter: "test/big",
	}
}

// TestOCREndToEnd 走完整链路:multipart 上传 → 双引擎并发 → 共识融合 → JSON。
func TestOCREndToEnd(t *testing.T) {
	upstream, auths := fakeUpstream("第一句。\n第二句。")
	defer upstream.Close()

	srv := &Server{Config: serverConfig(upstream.URL, "server-key"), Timeout: 30 * time.Second}
	rec := postOCR(t, srv.Handler(), testPNG(t), map[string]string{
		"engines": "test/tiny-a,test/tiny-b",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}

	var final arbiter.Final
	if err := json.Unmarshal(rec.Body.Bytes(), &final); err != nil {
		t.Fatal(err)
	}
	if final.Text != "第一句。\n第二句。" {
		t.Errorf("text = %q", final.Text)
	}
	// 两个引擎逐字一致,两行都是 2/2 共识;置信度由结构信号推导,不来自模型
	if final.Confidence != 0.9239 {
		t.Errorf("confidence = %v, want 0.9239 (2-of-2 consensus)", final.Confidence)
	}
	if s := final.Stats; s.Engines != 2 || s.ConsensusSegments != 2 || s.FailedEngines != 0 {
		t.Errorf("stats = %+v", s)
	}
	for _, l := range final.Segments {
		if l.Source != arbiter.SourceConsensus {
			t.Errorf("line %+v, want consensus", l)
		}
	}
	// Provider 密钥只来自服务端配置。
	for _, a := range auths() {
		if a != "Bearer server-key" {
			t.Errorf("Authorization = %q, want server key", a)
		}
	}
}

func TestOCRStreamEmitsDeltasThenResult(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if !request.Stream {
			t.Error("upstream request did not enable streaming")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"\",\"reasoning_content\":\"considering\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := &Server{Config: serverConfig(upstream.URL, "server-key"), Timeout: 30 * time.Second}
	body, contentType := multipartBody(t, testPNG(t), map[string]string{
		"engines": "test/tiny-a,test/tiny-b",
		"arbiter": "",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ocr/stream", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/x-ndjson") {
		t.Errorf("Content-Type = %q", got)
	}
	var events []streamEvent
	for _, line := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		var event streamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid stream line %q: %v", line, err)
		}
		events = append(events, event)
	}
	if len(events) < 4 || events[0].Type != "start" {
		t.Fatalf("events = %+v", events)
	}
	thinkingByAgent := make(map[string]string)
	outputByAgent := make(map[string]string)
	for _, event := range events[1 : len(events)-1] {
		if event.Type != "delta" || event.Stage != pipeline.StageEngine || event.Agent == "" {
			t.Errorf("unexpected progress event: %+v", event)
		}
		switch event.Kind {
		case string(agents.StreamThinking):
			thinkingByAgent[event.Agent] += event.Text
		case string(agents.StreamOutput):
			outputByAgent[event.Agent] += event.Text
		default:
			t.Errorf("unexpected delta kind: %+v", event)
		}
	}
	if len(thinkingByAgent) != 2 || len(outputByAgent) != 2 {
		t.Fatalf("thinking = %v, output = %v", thinkingByAgent, outputByAgent)
	}
	for agent, thinking := range thinkingByAgent {
		if thinking != "considering" || outputByAgent[agent] != "hello world" {
			t.Errorf("agent %q: thinking = %q, output = %q", agent, thinking, outputByAgent[agent])
		}
	}
	last := events[len(events)-1]
	if last.Type != "result" || last.Result == nil || last.Result.Text != "hello world" {
		t.Errorf("last event = %+v", last)
	}
	if strings.Contains(last.Result.Text, "considering") {
		t.Errorf("thinking leaked into result: %q", last.Result.Text)
	}
}

func TestManualArbitrationStream(t *testing.T) {
	var prompt string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		prompt = string(request.Messages[len(request.Messages)-1].Content)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"#3 正确句。\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	disputes, _ := json.Marshal([]arbiter.Dispute{{
		Segment: 3, Before: "前句。", After: "后句。",
		Candidates: []arbiter.Candidate{{Agent: "a", Text: "正确句。"}, {Agent: "b", Text: "正确信。"}},
	}})
	body, contentType := multipartBody(t, testPNG(t), map[string]string{
		"arbiter": "test/big", "disputes": string(disputes),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/arbitrate/stream", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	(&Server{Config: serverConfig(upstream.URL, "server-key")}).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var events []streamEvent
	for _, line := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		var event streamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	last := events[len(events)-1]
	if last.Type != "result" || len(last.Resolutions) != 1 {
		t.Fatalf("events = %+v", events)
	}
	resolution := last.Resolutions[0]
	if resolution.Segment != 3 || resolution.Text != "正确句。" || resolution.Confidence != 0.95 || len(resolution.From) != 1 {
		t.Fatalf("resolution = %+v", resolution)
	}
	for _, want := range []string{"#3", "前句。", "后句。", "正确句。", "正确信。"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q: %s", want, prompt)
		}
	}
}

// TestOCRRequestCannotOverrideProviderConnection verifies that browser fields
// cannot redirect requests or replace server-side credentials.
func TestOCRRequestCannotOverrideProviderConnection(t *testing.T) {
	upstream, auths := fakeUpstream("x")
	defer upstream.Close()

	srv := &Server{Config: serverConfig(upstream.URL, "server-key")}
	rec := postOCR(t, srv.Handler(), testPNG(t), map[string]string{
		"engines":  "test/tiny-a",
		"arbiter":  "",
		"base_url": "http://127.0.0.1:1",
		"api_key":  "user-key",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	got := auths()
	if len(got) != 1 || got[0] != "Bearer server-key" {
		t.Errorf("auths = %v, want server key", got)
	}
}

func TestOCRRejectsUnconfiguredModel(t *testing.T) {
	srv := &Server{Config: serverConfig("http://127.0.0.1:1", "")}
	rec := postOCR(t, srv.Handler(), testPNG(t), map[string]string{"engines": "other/model"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "未配置模型") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
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
		Config:  serverConfig("http://example/v1", "secret"),
		Timeout: 45 * time.Second,
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
	if len(cfg.Engines) != 2 || cfg.Arbiter != "test/big" || len(cfg.Providers) != 1 {
		t.Errorf("config = %+v", cfg)
	}
	provider := cfg.Providers[0]
	if provider.BaseURL != "http://example/v1" || !provider.HasAPIKey || len(provider.Models) != 3 {
		t.Errorf("provider = %+v", provider)
	}
	if cfg.TimeoutMS != (45 * time.Second).Milliseconds() {
		t.Errorf("timeout_ms = %d, want 45s", cfg.TimeoutMS)
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

// TestOCREscalationEndToEnd 验证全文切句、分歧提示词与仲裁回包格式。
func TestOCREscalationEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var arbiterPrompt string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		content := map[string]string{
			"tiny-a": "共同内容。\n发票编号inv0ice 042。",
			"tiny-b": "共同内容。发票编号invoice O42。",
			"big":    "#1 发票编号invoice 042。",
		}[req.Model]
		if req.Model == "big" {
			mu.Lock()
			arbiterPrompt = string(req.Messages[len(req.Messages)-1].Content)
			mu.Unlock()
		}
		body, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	srv := &Server{Config: serverConfig(upstream.URL, "server-key"), Timeout: 30 * time.Second}
	rec := postOCR(t, srv.Handler(), testPNG(t), map[string]string{
		"engines": "test/tiny-a,test/tiny-b",
		"arbiter": "test/big",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}

	var final arbiter.Final
	if err := json.Unmarshal(rec.Body.Bytes(), &final); err != nil {
		t.Fatal(err)
	}
	if final.Text != "共同内容。\n发票编号invoice 042。" {
		t.Errorf("text = %q", final.Text)
	}
	if s := final.Stats; s.Segments != 2 || s.ConsensusSegments != 1 || s.EscalatedSegments != 1 || s.EscalationErr != "" {
		t.Errorf("stats = %+v, want 1 consensus + 1 escalated", s)
	}
	// 仲裁裁定不同于任何候选(两家都读错了),没有旁证 → solo 档
	if l := final.Segments[1]; l.Source != arbiter.SourceEscalated || l.Confidence != 0.80 {
		t.Errorf("escalated line = %+v, want solo-tier confidence", l)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"#1", "共同内容。", "inv0ice 042", "invoice O42"} {
		if !strings.Contains(arbiterPrompt, want) {
			t.Errorf("arbiter prompt missing %q, got %s", want, arbiterPrompt)
		}
	}
}
