package pipeline

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/lieyanc/BetterOCR/internal/model"
)

func TestSplitList(t *testing.T) {
	if got := SplitList(" a, ,b ,,c "); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("SplitList = %v", got)
	}
	if got := SplitList(""); len(got) != 0 {
		t.Errorf("SplitList(\"\") = %v, want empty", got)
	}
}

// captureTransport 记录请求 URL 并统一返回 500,让引擎自然失败。
type captureTransport struct{ urls []string }

func (c *captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.urls = append(c.urls, r.URL.String())
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("boom")),
	}, nil
}

func TestRunUsesResolvedProviderEndpoint(t *testing.T) {
	ct := &captureTransport{}
	// PNG 魔数让 DetectContentType 通过,请求才会真正发出。
	pngMagic := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	_, err := Run(context.Background(), Config{
		Engines: []model.Resolved{{
			Ref: "local/m", ProviderID: "local", BaseURL: "http://local.test/v1",
			ID: "m", Context: 32768, Alias: "M", API: model.APIOpenAIChatCompletions,
		}},
		HTTPClient: &http.Client{Transport: ct},
	}, pngMagic)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ct.urls) == 0 || ct.urls[0] != "http://local.test/v1/chat/completions" {
		t.Errorf("请求 URL = %v", ct.urls)
	}
}

func TestRunRequiresEngines(t *testing.T) {
	if _, err := Run(context.Background(), Config{}, nil); err == nil {
		t.Error("Run without engines should error")
	}
}
