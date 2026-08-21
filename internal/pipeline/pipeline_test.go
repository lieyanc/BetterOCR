package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestRunStartsArbitrationAfterEveryEngineCompletes(t *testing.T) {
	var upstreamMu sync.Mutex
	engineStreamsDone := 0
	arbitrationStartedEarly := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if request.Model == "arbiter" {
			upstreamMu.Lock()
			arbitrationStartedEarly = engineStreamsDone != 2
			upstreamMu.Unlock()
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"#0 合同金额102元。\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}

		text := "合同金额100元。"
		if request.Model == "engine-b" {
			text = "合同金额101元。"
			time.Sleep(30 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\""+text+"\"}}]}\n\n")
		upstreamMu.Lock()
		engineStreamsDone++
		upstreamMu.Unlock()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	resolved := func(id string) model.Resolved {
		return model.Resolved{
			Ref: "test/" + id, ProviderID: "test", ProviderAlias: "Test",
			BaseURL: upstream.URL, ID: id, Alias: id, Context: 32768,
			API: model.APIOpenAIChatCompletions,
		}
	}
	arbiterModel := resolved("arbiter")
	var events []Event
	final, err := Run(context.Background(), Config{
		Engines: []model.Resolved{resolved("engine-a"), resolved("engine-b")},
		Arbiter: &arbiterModel,
		OnEvent: func(event Event) { events = append(events, event) },
	}, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	if err != nil {
		t.Fatal(err)
	}
	if arbitrationStartedEarly {
		t.Fatal("arbiter request started before every engine stream completed")
	}
	if final.Stats.EscalatedSegments != 1 || final.Text != "合同金额102元。" {
		t.Fatalf("final = %+v", final)
	}

	arbiterStart := -1
	engineStageDone := -1
	engineDone := 0
	for index, event := range events {
		if event.Type == EventAgentDone && event.Stage == StageEngine {
			engineDone++
		}
		if event.Type == EventStageDone && event.Stage == StageEngine {
			engineStageDone = index
		}
		if event.Type == EventAgentStart && event.Stage == StageArbiter {
			arbiterStart = index
			if engineDone != 2 {
				t.Fatalf("arbiter event at %d after only %d engine completions: %+v", index, engineDone, events)
			}
			break
		}
	}
	if arbiterStart < 0 || engineStageDone < 0 || engineStageDone > arbiterStart {
		t.Fatalf("invalid engine/arbiter lifecycle order: %+v", events)
	}
}

func TestRunRetriesOnlyFailedEngineBeforeArbitration(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	arbitrationStartedEarly := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		calls[request.Model]++
		attempt := calls[request.Model]
		if request.Model == "arbiter" {
			arbitrationStartedEarly = calls["engine-a"] != 2 || calls["engine-b"] != 1
		}
		mu.Unlock()

		if request.Model == "engine-a" && attempt == 1 {
			<-r.Context().Done()
			return
		}
		content := map[string]string{
			"engine-a": "合同金额100元。",
			"engine-b": "合同金额101元。",
			"arbiter":  "#0 合同金额102元。",
		}[request.Model]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
	}))
	defer upstream.Close()

	resolved := func(id string) model.Resolved {
		return model.Resolved{
			Ref: "test/" + id, ProviderID: "test", ProviderAlias: "Test",
			BaseURL: upstream.URL, ID: id, Alias: id, Context: 32768,
			API: model.APIOpenAIChatCompletions,
		}
	}
	arbiterModel := resolved("arbiter")
	var attempts []Event
	final, err := Run(context.Background(), Config{
		Engines:       []model.Resolved{resolved("engine-a"), resolved("engine-b")},
		Arbiter:       &arbiterModel,
		EngineTimeout: 40 * time.Millisecond, ArbiterTimeout: time.Second,
		EngineMaxAttempts: 2, ArbiterMaxAttempts: 1,
		OnEvent: func(event Event) {
			if event.Type == EventAttemptStart {
				attempts = append(attempts, event)
			}
		},
	}, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["engine-a"] != 2 || calls["engine-b"] != 1 || calls["arbiter"] != 1 {
		t.Fatalf("calls = %v", calls)
	}
	if arbitrationStartedEarly {
		t.Fatal("arbitration started before the retried engine reached its terminal state")
	}
	if final.Text != "合同金额102元。" {
		t.Fatalf("final text = %q", final.Text)
	}
	var engineAAttempts []int
	for _, event := range attempts {
		if event.Stage == StageEngine && strings.Contains(event.Agent, "engine-a") {
			engineAAttempts = append(engineAAttempts, event.Attempt)
		}
	}
	if !reflect.DeepEqual(engineAAttempts, []int{1, 2}) {
		t.Fatalf("engine-a attempt events = %v", engineAAttempts)
	}
}

func TestRunRetriesArbitrationWithoutRerunningEngines(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		calls[request.Model]++
		attempt := calls[request.Model]
		mu.Unlock()
		if request.Model == "arbiter" && attempt == 1 {
			<-r.Context().Done()
			return
		}
		content := map[string]string{
			"engine-a": "发票编号042。",
			"engine-b": "发票编号O42。",
			"arbiter":  "#0 发票编号042。",
		}[request.Model]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
	}))
	defer upstream.Close()

	resolved := func(id string) model.Resolved {
		return model.Resolved{
			Ref: "test/" + id, ProviderID: "test", ProviderAlias: "Test",
			BaseURL: upstream.URL, ID: id, Alias: id, Context: 32768,
			API: model.APIOpenAIChatCompletions,
		}
	}
	arbiterModel := resolved("arbiter")
	final, err := Run(context.Background(), Config{
		Engines:       []model.Resolved{resolved("engine-a"), resolved("engine-b")},
		Arbiter:       &arbiterModel,
		EngineTimeout: time.Second, ArbiterTimeout: 40 * time.Millisecond,
		EngineMaxAttempts: 1, ArbiterMaxAttempts: 2,
	}, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["engine-a"] != 1 || calls["engine-b"] != 1 || calls["arbiter"] != 2 {
		t.Fatalf("calls = %v", calls)
	}
	if final.Text != "发票编号042。" || final.Stats.EscalatedSegments != 1 {
		t.Fatalf("final = %+v", final)
	}
}

func TestRunRenewsIndependentTimeoutsWhileModelsAreStreaming(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		calls[request.Model]++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("test server does not support flushing")
		}
		chunks := map[string][]string{
			"engine-a": {"合同", "金额", "100", "元。"},
			"engine-b": {"合同金额101元。"},
			"arbiter":  {"#0 ", "合同", "金额", "102", "元。"},
		}[request.Model]
		for index, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", chunk)
			flusher.Flush()
			if index < len(chunks)-1 {
				time.Sleep(30 * time.Millisecond)
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	resolved := func(id string) model.Resolved {
		return model.Resolved{
			Ref: "test/" + id, ProviderID: "test", ProviderAlias: "Test",
			BaseURL: upstream.URL, ID: id, Alias: id, Context: 32768,
			API: model.APIOpenAIChatCompletions,
		}
	}
	arbiterModel := resolved("arbiter")
	final, err := Run(context.Background(), Config{
		Engines:       []model.Resolved{resolved("engine-a"), resolved("engine-b")},
		Arbiter:       &arbiterModel,
		EngineTimeout: 70 * time.Millisecond, ArbiterTimeout: 70 * time.Millisecond,
		EngineMaxAttempts: 1, ArbiterMaxAttempts: 1,
	}, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["engine-a"] != 1 || calls["engine-b"] != 1 || calls["arbiter"] != 1 {
		t.Fatalf("streaming attempts were interrupted: calls = %v", calls)
	}
	if final.Text != "合同金额102元。" {
		t.Fatalf("final text = %q", final.Text)
	}
}

func TestRunRetriesWhenStreamingBecomesIdle(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		attempt := calls
		mu.Unlock()
		if attempt == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"部分\"}}]}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "完整结果。"}}},
		})
	}))
	defer upstream.Close()

	engine := model.Resolved{
		Ref: "test/engine", ProviderID: "test", BaseURL: upstream.URL,
		ID: "engine", Alias: "engine", Context: 32768, API: model.APIOpenAIChatCompletions,
	}
	final, err := Run(context.Background(), Config{
		Engines: []model.Resolved{engine}, EngineTimeout: 50 * time.Millisecond, EngineMaxAttempts: 2,
	}, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if final.Text != "完整结果。" {
		t.Fatalf("final text = %q", final.Text)
	}
}

func TestRunParentCancellationStopsFurtherAttempts(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer upstream.Close()

	engine := model.Resolved{
		Ref: "test/engine", ProviderID: "test", BaseURL: upstream.URL,
		ID: "engine", Alias: "engine", Context: 32768, API: model.APIOpenAIChatCompletions,
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	final, err := Run(ctx, Config{
		Engines: []model.Resolved{engine}, EngineTimeout: time.Second, EngineMaxAttempts: 3,
	}, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("engine calls after parent cancellation = %d, want 1", calls)
	}
	if len(final.Candidates) != 1 || !strings.Contains(final.Candidates[0].Err, context.Canceled.Error()) {
		t.Fatalf("final candidates = %+v", final.Candidates)
	}
}
