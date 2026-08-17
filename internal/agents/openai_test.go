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
	const content = "Sure! ```json\n[{\"text\":\"你好,世界\",\"confidence\":0.91},{\"text\":\"   \",\"confidence\":0.5},{\"text\":\"second line\",\"confidence\":0.82}]\n```"
	tests := []struct {
		name      string
		api       model.API
		wantPath  string
		response  any
		checkAuth func(*testing.T, *http.Request)
		checkBody func(*testing.T, map[string]any)
	}{
		{
			name: "openai chat", api: model.APIOpenAIChat, wantPath: "/chat/completions",
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
			name: "anthropic", api: model.APIAnthropic, wantPath: "/messages",
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
			if len(result.Lines) != 2 || result.Lines[0].Text != "你好,世界" || result.Lines[1].Confidence != 0.82 {
				t.Errorf("lines = %+v", result.Lines)
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
		_, _ = w.Write(chatContent(`[{"text":"x","confidence":1}]`))
	}))
	defer srv.Close()

	vlm := NewVisionVLM("local#1", resolved(model.APIOpenAIChat, srv.URL, ""), nil)
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

	vlm := NewVisionVLM("t#1", resolved(model.APIAnthropic, srv.URL, ""), nil)
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

	vlm := NewVisionVLM("t#1", resolved(model.APIOpenAIChat, srv.URL, ""), nil)
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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output_text": "```json\n[{\"row\":3,\"text\":\"fixed line\",\"confidence\":0.97}]\n```",
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
			{Agent: "a#1", Text: "f1xed line", Confidence: 0.7},
			{Agent: "b#1", Text: "fixed 1ine", Confidence: 0.8},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"row 3", "ctx above", "ctx below", "f1xed line", "fixed 1ine", "data:image/png;base64,"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body missing %q", want)
		}
	}
	if len(rs) != 1 || rs[0] != (arbiter.Resolution{Row: 3, Text: "fixed line", Confidence: 0.97}) {
		t.Errorf("resolutions = %+v", rs)
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
		{in: "]] then [[", wantErr: true},
	}
	for _, test := range cases {
		got, err := extractJSON(test.in)
		if test.wantErr {
			if err == nil {
				t.Errorf("extractJSON(%q) = %q, want error", test.in, got)
			}
			continue
		}
		if err != nil || string(got) != test.want {
			t.Errorf("extractJSON(%q) = %q, %v; want %q", test.in, got, err, test.want)
		}
	}
}
