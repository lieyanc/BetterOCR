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
	"reflect"
	"strings"
	"testing"

	"github.com/lieyanc/BetterOCR/internal/arbiter"
	"github.com/lieyanc/BetterOCR/internal/model"
)

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

func resolved(api model.API, baseURL, key string) model.Resolved {
	return model.Resolved{
		Ref: "provider/vision", ProviderID: "provider", BaseURL: baseURL,
		APIKey: key, ID: "vision-id", Context: 4096, Alias: "Vision", API: api,
	}
}

func chatContent(content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
	})
	return b
}

func TestVLMRecognizeAcrossAPIs(t *testing.T) {
	// 带围栏、含空行的纯文本输出:围栏应被剥掉,空行不成行
	const content = "```\n你好,世界\n   \n second line \n```"
	tests := []struct {
		name      string
		api       model.API
		wantPath  string
		response  any
		checkAuth func(*testing.T, *http.Request)
		checkBody func(*testing.T, map[string]any)
	}{
		{
			name: "openai chat", api: model.APIOpenAIChatCompletions, wantPath: "/chat/completions",
			response: map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}},
			checkAuth: func(t *testing.T, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
					t.Errorf("Authorization = %q", got)
				}
			},
			checkBody: func(t *testing.T, body map[string]any) {
				if body["model"] != "vision-id" || body["max_tokens"] != float64(2048) {
					t.Errorf("chat body = %+v", body)
				}
				if !strings.Contains(mustJSON(body["messages"]), "data:image/png;base64,") {
					t.Error("chat request missing image data URL")
				}
			},
		},
		{
			name: "openai responses", api: model.APIOpenAIResponses, wantPath: "/responses",
			response: map[string]any{"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": content}}}}},
			checkAuth: func(t *testing.T, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
					t.Errorf("Authorization = %q", got)
				}
			},
			checkBody: func(t *testing.T, body map[string]any) {
				if body["max_output_tokens"] != float64(2048) || !strings.Contains(mustJSON(body["input"]), "input_image") {
					t.Errorf("responses body = %+v", body)
				}
			},
		},
		{
			name: "anthropic", api: model.APIAnthropicMessages, wantPath: "/messages",
			response: map[string]any{"content": []any{map[string]any{"type": "text", "text": content}}},
			checkAuth: func(t *testing.T, r *http.Request) {
				if got := r.Header.Get("x-api-key"); got != "test-key" {
					t.Errorf("x-api-key = %q", got)
				}
				if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
					t.Errorf("anthropic-version = %q", got)
				}
				if got := r.Header.Get("Authorization"); got != "" {
					t.Errorf("unexpected Authorization = %q", got)
				}
			},
			checkBody: func(t *testing.T, body map[string]any) {
				encoded := mustJSON(body["messages"])
				if body["max_tokens"] != float64(2048) || !strings.Contains(encoded, `"media_type":"image/png"`) || strings.Contains(encoded, "data:image") {
					t.Errorf("anthropic body = %+v", body)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.wantPath {
					t.Errorf("path = %q, want %q", r.URL.Path, tt.wantPath)
				}
				tt.checkAuth(t, r)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				tt.checkBody(t, body)
				_ = json.NewEncoder(w).Encode(tt.response)
			}))
			defer srv.Close()

			vlm := NewVisionVLM("vision#1", resolved(tt.api, srv.URL, "test-key"), nil)
			if vlm.Name() != "vision#1" {
				t.Errorf("Name = %q", vlm.Name())
			}
			result, err := vlm.Recognize(context.Background(), testPNG(t))
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Lines) != 2 || result.Lines[0] != "你好,世界" || result.Lines[1] != "second line" {
				t.Errorf("lines = %q", result.Lines)
			}
		})
	}
}

func mustJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func TestVLMNoKeySendsNoAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write(chatContent("x"))
	}))
	defer srv.Close()

	vlm := NewVisionVLM("local#1", resolved(model.APIOpenAIChatCompletions, srv.URL, ""), nil)
	if _, err := vlm.Recognize(context.Background(), testPNG(t)); err != nil {
		t.Fatal(err)
	}
}

func TestVLMHTTPErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"message":"quota exceeded"}}`))
	}))
	defer srv.Close()

	vlm := NewVisionVLM("t#1", resolved(model.APIAnthropicMessages, srv.URL, ""), nil)
	_, err := vlm.Recognize(context.Background(), testPNG(t))
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") || !strings.Contains(err.Error(), "429") {
		t.Errorf("err = %v, want HTTP 429 with server message", err)
	}
}

func TestVLMAPIErrorInSuccessfulHTTPPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"message":"model unavailable"}}`))
	}))
	defer srv.Close()

	vlm := NewVisionVLM("t#1", resolved(model.APIOpenAIChatCompletions, srv.URL, ""), nil)
	_, err := vlm.Recognize(context.Background(), testPNG(t))
	if err == nil || !strings.Contains(err.Error(), "model unavailable") {
		t.Errorf("err = %v", err)
	}
}

func TestVLMRejectsNonImage(t *testing.T) {
	vlm := NewVisionVLM("t#1", resolved(model.APIOpenAIResponses, "http://127.0.0.1:0", ""), nil)
	_, err := vlm.Recognize(context.Background(), []byte("plain text, not an image"))
	if err == nil || !strings.Contains(err.Error(), "图片格式") {
		t.Errorf("err = %v, want image-format rejection", err)
	}
}

func TestEscalatorResolve(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		gotBody = buf.String()
		// 夹带寒暄与围栏,以及一行判定"图中不存在"的空裁定
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output_text": "Sure, here you go:\n```\n#3 fixed line\n#4\n```",
		})
	}))
	defer srv.Close()

	esc := NewVisionEscalator(resolved(model.APIOpenAIResponses, srv.URL, ""), nil)
	if esc.Name() != "arbiter:Vision (provider)" {
		t.Errorf("Name = %q", esc.Name())
	}
	rs, err := esc.Resolve(context.Background(), testPNG(t), []arbiter.Dispute{{
		Row: 3, Before: "ctx above", After: "ctx below",
		Candidates: []arbiter.Candidate{
			{Agent: "a#1", Text: "f1xed line"},
			{Agent: "b#1", Text: "fixed 1ine"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#3", "ctx above", "ctx below", "f1xed line", "fixed 1ine", "data:image/png;base64,"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body missing %q", want)
		}
	}
	// 候选清单不得夹带票数/置信度之类的人气信号,否则与"看图判断"的要求自相矛盾
	if strings.Contains(gotBody, "confidence") {
		t.Error("dispute prompt must not hand the arbiter a confidence signal")
	}
	want := []arbiter.Resolution{{Row: 3, Text: "fixed line"}, {Row: 4, Text: ""}}
	if !reflect.DeepEqual(rs, want) {
		t.Errorf("resolutions = %+v, want %+v", rs, want)
	}
}

// TestEscalatorRejectsUnparseableOutput 锁定失败必须显式:一行都认不出来时
// 报错,让 Stats.EscalationErr 记录下来,而不是静默返回空让分歧行悄悄兜底。
func TestEscalatorRejectsUnparseableOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output_text": "I could not read the image clearly.",
		})
	}))
	defer srv.Close()

	esc := NewVisionEscalator(resolved(model.APIOpenAIResponses, srv.URL, ""), nil)
	_, err := esc.Resolve(context.Background(), testPNG(t), []arbiter.Dispute{{Row: 0}})
	if err == nil || !strings.Contains(err.Error(), "#<row>") {
		t.Errorf("err = %v, want a parse failure mentioning the expected form", err)
	}
}

func TestSplitLines(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
	}{
		{name: "plain", in: "alpha\nbravo", want: []string{"alpha", "bravo"}},
		{name: "fenced", in: "```text\nalpha\nbravo\n```", want: []string{"alpha", "bravo"}},
		{name: "blank and padded", in: "  alpha  \n\n\t\nbravo\n", want: []string{"alpha", "bravo"}},
		{name: "no text in image", in: "", want: []string{}},
		{name: "inner backticks kept", in: "a ``` b", want: []string{"a ``` b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitLines(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitLines(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseRowLine(t *testing.T) {
	cases := []struct {
		in       string
		wantRow  int
		wantText string
		wantOK   bool
	}{
		{in: "#3 fixed line", wantRow: 3, wantText: "fixed line", wantOK: true},
		{in: "  #12\ttab separated  ", wantRow: 12, wantText: "tab separated", wantOK: true},
		{in: "#0", wantRow: 0, wantText: "", wantOK: true}, // 判定该行不存在于图中
		{in: "#7   ", wantRow: 7, wantText: "", wantOK: true},
		// 没有分隔空白时不猜行号:整行忽略,好过把 "#1234.5 元" 读成 row 1234
		{in: "#1234.5 元", wantOK: false},
		{in: "row 3: text", wantOK: false},
		{in: "#", wantOK: false},
		{in: "", wantOK: false},
		{in: "Sure, here you go:", wantOK: false},
	}
	for _, tc := range cases {
		row, text, ok := parseRowLine(tc.in)
		if ok != tc.wantOK || (ok && (row != tc.wantRow || text != tc.wantText)) {
			t.Errorf("parseRowLine(%q) = %d, %q, %v; want %d, %q, %v",
				tc.in, row, text, ok, tc.wantRow, tc.wantText, tc.wantOK)
		}
	}
}
