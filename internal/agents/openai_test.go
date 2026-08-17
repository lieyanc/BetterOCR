package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lieyanc/BetterOCR/internal/arbiter"
)

// testPNG 在测试内生成一张最小合法 PNG,避免任何外部测试资产。
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

// chatContent 构造一个 /chat/completions 成功响应体。
func chatContent(content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
	})
	return b
}

func TestVLMRecognize(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		gotBody, _ = json.Marshal(func() any { var v any; json.NewDecoder(r.Body).Decode(&v); return v }())
		// 模型偶尔加围栏与说明文字,客户端必须容忍
		w.Write(chatContent("Sure! Here it is:\n```json\n[{\"text\":\"你好,世界\",\"confidence\":0.91},{\"text\":\"   \",\"confidence\":0.5},{\"text\":\"second line\",\"confidence\":0.82}]\n```"))
	}))
	defer srv.Close()

	vlm := NewOpenAIVLM("tiny#1", "tiny-vlm", OpenAIConfig{BaseURL: srv.URL, APIKey: "test-key"})
	if vlm.Name() != "tiny#1" {
		t.Errorf("Name = %q", vlm.Name())
	}
	res, err := vlm.Recognize(context.Background(), testPNG(t))
	if err != nil {
		t.Fatal(err)
	}

	body := string(gotBody)
	if !strings.Contains(body, `"tiny-vlm"`) {
		t.Error("request missing model name")
	}
	if !strings.Contains(body, "data:image/png;base64,") {
		t.Error("request missing image data URL")
	}

	if len(res.Lines) != 2 { // 空白行被丢弃
		t.Fatalf("lines = %+v, want 2", res.Lines)
	}
	if res.Lines[0].Text != "你好,世界" || res.Lines[0].Confidence != 0.91 {
		t.Errorf("lines[0] = %+v", res.Lines[0])
	}
	if res.Lines[1].Text != "second line" || res.Lines[1].Confidence != 0.82 {
		t.Errorf("lines[1] = %+v", res.Lines[1])
	}
}

func TestVLMNoKeySendsNoAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty for keyless local server", got)
		}
		w.Write(chatContent(`[{"text":"x","confidence":1}]`))
	}))
	defer srv.Close()

	vlm := NewOpenAIVLM("local#1", "local", OpenAIConfig{BaseURL: srv.URL})
	if _, err := vlm.Recognize(context.Background(), testPNG(t)); err != nil {
		t.Fatal(err)
	}
}

func TestVLMHTTPErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
	}))
	defer srv.Close()

	vlm := NewOpenAIVLM("t#1", "t", OpenAIConfig{BaseURL: srv.URL})
	_, err := vlm.Recognize(context.Background(), testPNG(t))
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") || !strings.Contains(err.Error(), "429") {
		t.Errorf("err = %v, want HTTP 429 with server message", err)
	}
}

func TestVLMRejectsNonImage(t *testing.T) {
	vlm := NewOpenAIVLM("t#1", "t", OpenAIConfig{BaseURL: "http://127.0.0.1:0"})
	_, err := vlm.Recognize(context.Background(), []byte("plain text, not an image"))
	if err == nil || !strings.Contains(err.Error(), "图片格式") {
		t.Errorf("err = %v, want image-format rejection before any HTTP call", err)
	}
}

func TestEscalatorResolve(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		buf.ReadFrom(r.Body)
		gotBody = buf.String()
		w.Write(chatContent("```json\n[{\"row\":3,\"text\":\"fixed line\",\"confidence\":0.97}]\n```"))
	}))
	defer srv.Close()

	esc := NewOpenAIEscalator("strong-vlm", OpenAIConfig{BaseURL: srv.URL})
	if esc.Name() != "arbiter:strong-vlm" {
		t.Errorf("Name = %q", esc.Name())
	}
	rs, err := esc.Resolve(context.Background(), testPNG(t), []arbiter.Dispute{{
		Row:    3,
		Before: "ctx above",
		After:  "ctx below",
		Candidates: []arbiter.Candidate{
			{Agent: "a#1", Text: "f1xed line", Confidence: 0.7},
			{Agent: "b#1", Text: "fixed 1ine", Confidence: 0.8},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	// 分歧提示词必须带上行号、上下文与各候选,仲裁模型才能在图中定位
	for _, want := range []string{"row 3", "ctx above", "ctx below", "f1xed line", "fixed 1ine", "data:image/png;base64,"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body missing %q", want)
		}
	}
	want := []arbiter.Resolution{{Row: 3, Text: "fixed line", Confidence: 0.97}}
	if len(rs) != 1 || rs[0] != want[0] {
		t.Errorf("resolutions = %+v, want %+v", rs, want)
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "```json\n[1,2]\n```", want: "[1,2]"},
		{in: "prefix [\"a\"] suffix", want: "[\"a\"]"},
		{in: "no array here", wantErr: true},
		{in: "]] then [[", wantErr: true}, // 只有倒序括号,不构成数组
	}
	for _, c := range cases {
		got, err := extractJSON(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("extractJSON(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil || string(got) != c.want {
			t.Errorf("extractJSON(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}
