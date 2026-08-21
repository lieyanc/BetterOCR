package server

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lieyanc/BetterOCR/internal/arbiter"
	"github.com/lieyanc/BetterOCR/internal/database"
	"github.com/lieyanc/BetterOCR/internal/pipeline"
)

type fakePageRenderer struct {
	pages int
}

func (renderer *fakePageRenderer) Render(
	ctx context.Context,
	_ string,
	onCount func(int) error,
	shouldRender func(int) bool,
	onPage func(int, image.Image) error,
) error {
	if err := onCount(renderer.pages); err != nil {
		return err
	}
	for index := 0; index < renderer.pages; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if shouldRender != nil && !shouldRender(index) {
			continue
		}
		page := image.NewRGBA(image.Rect(0, 0, 32+index, 48+index))
		page.Set(0, 0, color.White)
		if err := onPage(index, page); err != nil {
			return err
		}
	}
	return nil
}

func (*fakePageRenderer) Close() error { return nil }

func TestDocumentUploadPreparationOwnershipAndPersistence(t *testing.T) {
	root := t.TempDir()
	upstream, _ := fakeUpstream("server-side OCR result")
	defer upstream.Close()
	cfg := serverConfig(upstream.URL, "server-key")
	cfg.ServeAddr = "127.0.0.1:8787"
	store, err := database.Open(filepath.Join(root, "database.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitializeAdmin("owner", "owner-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser("other", "other-password", database.RoleUser); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Config: cfg, Store: store, DocumentRoot: filepath.Join(root, "documents"),
		PDFRenderer: &fakePageRenderer{pages: 3},
	}
	handler := srv.Handler()
	ownerCookie, ownerCSRF := loginForTest(t, handler, "owner", "owner-password")
	otherCookie, _ := loginForTest(t, handler, "other", "other-password")

	upload := requestWithAuth(
		http.MethodPost,
		"/api/documents?filename=contract.pdf&engines=test%2Ftiny-a&arbiter=&auto_arbitrate=false",
		bytes.NewReader([]byte("%PDF-1.4\nserver-side-test")),
		ownerCookie,
		ownerCSRF,
	)
	upload.Header.Set("Content-Type", "application/pdf")
	uploadRecorder := httptest.NewRecorder()
	handler.ServeHTTP(uploadRecorder, upload)
	if uploadRecorder.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", uploadRecorder.Code, uploadRecorder.Body.String())
	}
	var created database.DocumentProject
	if err := json.Unmarshal(uploadRecorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Status != database.DocumentPreparing || created.SizeBytes == 0 {
		t.Fatalf("created = %+v", created)
	}

	document := waitForDocumentStatus(t, handler, created.ID, ownerCookie, database.DocumentReady)
	if document.PageCount != 3 || document.PreparedPages != 3 || len(document.Pages) != 3 {
		t.Fatalf("prepared document = %+v", document)
	}
	for _, page := range document.Pages {
		if !page.ImageReady || page.Status != database.PageQueued {
			t.Fatalf("page = %+v", page)
		}
		if _, err := os.Stat(filepath.Join(root, "documents", document.ID, "pages", page.ID+".jpg")); err != nil {
			t.Fatal(err)
		}
	}

	forbidden := requestWithAuth(http.MethodGet, "/api/documents/"+document.ID, nil, otherCookie, "")
	forbiddenRecorder := httptest.NewRecorder()
	handler.ServeHTTP(forbiddenRecorder, forbidden)
	if forbiddenRecorder.Code != http.StatusForbidden {
		t.Fatalf("other user status=%d body=%s", forbiddenRecorder.Code, forbiddenRecorder.Body.String())
	}

	reversed := []string{document.Pages[2].ID, document.Pages[1].ID, document.Pages[0].ID}
	orderBody, _ := json.Marshal(map[string]any{"page_ids": reversed})
	orderRequest := requestWithAuth(
		http.MethodPut, "/api/documents/"+document.ID+"/pages/order",
		bytes.NewReader(orderBody), ownerCookie, ownerCSRF,
	)
	orderRequest.Header.Set("Content-Type", "application/json")
	orderRecorder := httptest.NewRecorder()
	handler.ServeHTTP(orderRecorder, orderRequest)
	if orderRecorder.Code != http.StatusOK {
		t.Fatalf("order status=%d body=%s", orderRecorder.Code, orderRecorder.Body.String())
	}

	runBody, _ := json.Marshal(map[string]any{
		"engines": []string{"test/tiny-a", "test/tiny-b"}, "arbiter": "", "auto_arbitrate": false,
	})
	runRequest := requestWithAuth(
		http.MethodPost, "/api/documents/"+document.ID+"/run",
		bytes.NewReader(runBody), ownerCookie, ownerCSRF,
	)
	runRequest.Header.Set("Content-Type", "application/json")
	runRecorder := httptest.NewRecorder()
	handler.ServeHTTP(runRecorder, runRequest)
	if runRecorder.Code != http.StatusAccepted {
		t.Fatalf("run status=%d body=%s", runRecorder.Code, runRecorder.Body.String())
	}
	document = waitForDocumentStatus(t, handler, document.ID, ownerCookie, database.DocumentCompleted)
	if document.ProcessedPages != 3 || document.FailedPages != 0 {
		t.Fatalf("processed document = %+v", document)
	}
	for _, page := range document.Pages {
		if !page.ResultReady || page.Status != database.PageCompleted {
			t.Fatalf("processed page = %+v", page)
		}
		if _, err := os.Stat(filepath.Join(root, "documents", document.ID, "results", page.ID+".json")); err != nil {
			t.Fatal(err)
		}
	}

	final := arbiter.Final{
		Text:       "result-must-not-live-in-database-json",
		Confidence: 0.9,
		Segments: []arbiter.FinalSegment{{
			Text: "待核对", Confidence: 0.6, Source: arbiter.SourceFallback, Disputed: true,
		}},
	}
	resultBody, _ := json.Marshal(final)
	resultRequest := requestWithAuth(
		http.MethodPut,
		"/api/documents/"+document.ID+"/pages/"+reversed[0]+"/result",
		bytes.NewReader(resultBody), ownerCookie, ownerCSRF,
	)
	resultRequest.Header.Set("Content-Type", "application/json")
	resultRecorder := httptest.NewRecorder()
	handler.ServeHTTP(resultRecorder, resultRequest)
	if resultRecorder.Code != http.StatusOK {
		t.Fatalf("result status=%d body=%s", resultRecorder.Code, resultRecorder.Body.String())
	}
	databaseJSON, err := os.ReadFile(filepath.Join(root, "database.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(databaseJSON, []byte(final.Text)) {
		t.Fatal("full page result leaked into database.json")
	}
	resultPath := filepath.Join(root, "documents", document.ID, "results", reversed[0]+".json")
	if resultJSON, err := os.ReadFile(resultPath); err != nil || !bytes.Contains(resultJSON, []byte(final.Text)) {
		t.Fatalf("result file err=%v body=%s", err, resultJSON)
	}

	reopened, err := database.Open(filepath.Join(root, "database.json"))
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := reopened.Document(document.ID)
	if !ok || persisted.Pages[0].ID != reversed[0] || persisted.PendingDisputes != 1 {
		t.Fatalf("persisted = %+v ok=%v", persisted, ok)
	}
}

func waitForDocumentStatus(
	t *testing.T,
	handler http.Handler,
	id string,
	cookie *http.Cookie,
	want string,
) database.DocumentProject {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		request := requestWithAuth(http.MethodGet, "/api/documents/"+id, nil, cookie, "")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("get document status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var document database.DocumentProject
		if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
		if document.Status == want {
			return document
		}
		if document.Status == database.DocumentFailed {
			t.Fatalf("document failed: %s", document.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("document did not reach %s", want)
	return database.DocumentProject{}
}

func TestDocumentProgressStreamsWaitingOutputAndFinalStats(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.Stream {
			http.Error(w, "expected streaming request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		controller := http.NewResponseController(w)
		_ = controller.Flush()
		time.Sleep(1100 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"实时输出。\"}}]}\n\n"))
		_ = controller.Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	root := t.TempDir()
	store, err := database.Open(filepath.Join(root, "database.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitializeAdmin("owner", "owner-password"); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Config: serverConfig(upstream.URL, "server-key"), Store: store,
		DocumentRoot: filepath.Join(root, "documents"),
	}
	handler := srv.Handler()
	cookie, csrf := loginForTest(t, handler, "owner", "owner-password")

	upload := requestWithAuth(
		http.MethodPost,
		"/api/documents?filename=page.png&engines=test%2Ftiny-a%2Ctest%2Ftiny-b&arbiter=&auto_arbitrate=false",
		bytes.NewReader(testPNG(t)), cookie, csrf,
	)
	upload.Header.Set("Content-Type", "image/png")
	uploadRecorder := httptest.NewRecorder()
	handler.ServeHTTP(uploadRecorder, upload)
	if uploadRecorder.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", uploadRecorder.Code, uploadRecorder.Body.String())
	}
	var document database.DocumentProject
	if err := json.Unmarshal(uploadRecorder.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	document = waitForDocumentStatus(t, handler, document.ID, cookie, database.DocumentReady)

	runBody, _ := json.Marshal(map[string]any{
		"engines": []string{"test/tiny-a", "test/tiny-b"}, "arbiter": "", "auto_arbitrate": false,
	})
	runRequest := requestWithAuth(
		http.MethodPost, "/api/documents/"+document.ID+"/run",
		bytes.NewReader(runBody), cookie, csrf,
	)
	runRequest.Header.Set("Content-Type", "application/json")
	runRecorder := httptest.NewRecorder()
	handler.ServeHTTP(runRecorder, runRequest)
	if runRecorder.Code != http.StatusAccepted {
		t.Fatalf("run status=%d body=%s", runRecorder.Code, runRecorder.Body.String())
	}

	progressRequest := requestWithAuth(
		http.MethodGet, "/api/documents/"+document.ID+"/events", nil, cookie, "",
	)
	progressRecorder := httptest.NewRecorder()
	handler.ServeHTTP(progressRecorder, progressRequest)
	if progressRecorder.Code != http.StatusOK {
		t.Fatalf("progress status=%d body=%s", progressRecorder.Code, progressRecorder.Body.String())
	}
	if contentType := progressRecorder.Header().Get("Content-Type"); !strings.Contains(contentType, "application/x-ndjson") {
		t.Fatalf("progress content type = %q", contentType)
	}

	var events []documentProgressEvent
	for _, line := range strings.Split(strings.TrimSpace(progressRecorder.Body.String()), "\n") {
		var event documentProgressEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid progress event %q: %v", line, err)
		}
		events = append(events, event)
	}
	var sawWaitingHeartbeat, sawOutputStats bool
	for _, event := range events {
		for _, agent := range event.Agents {
			if !agent.FirstToken && agent.ElapsedMS >= 900 {
				sawWaitingHeartbeat = true
			}
			if agent.FirstToken && agent.EstimatedTokens > 0 && agent.OutputChars > 0 && agent.TPS > 0 && strings.Contains(agent.Output, "实时输出") {
				sawOutputStats = true
			}
		}
	}
	if !sawWaitingHeartbeat || !sawOutputStats {
		t.Fatalf("waiting=%v output=%v events=%+v", sawWaitingHeartbeat, sawOutputStats, events)
	}
	last := events[len(events)-1]
	if last.DocumentStatus != database.DocumentCompleted || last.CompletedEngines != 2 {
		t.Fatalf("last progress = %+v", last)
	}
}

func TestDocumentProgressResetsFailedAttemptPreviewBeforeRetry(t *testing.T) {
	hub := newDocumentProgressHub()
	tracker := hub.beginPage("doc-1", database.DocumentPage{ID: "page-1", PageNumber: 1}, 1)
	tracker.handle(pipeline.Event{
		Type: pipeline.EventAgentStart, Stage: pipeline.StageEngine,
		Agent: "engine-a", MaxAttempts: 2,
	})
	tracker.handle(pipeline.Event{
		Type: pipeline.EventAttemptStart, Stage: pipeline.StageEngine,
		Agent: "engine-a", Attempt: 1, MaxAttempts: 2,
	})
	tracker.handle(pipeline.Event{
		Type: pipeline.EventDelta, Stage: pipeline.StageEngine,
		Agent: "engine-a", Kind: "output", Text: "失败尝试的文本",
	})
	tracker.handle(pipeline.Event{
		Type: pipeline.EventAttemptFailed, Stage: pipeline.StageEngine,
		Agent: "engine-a", Attempt: 1, MaxAttempts: 2, Error: "deadline exceeded",
	})
	tracker.handle(pipeline.Event{
		Type: pipeline.EventAttemptStart, Stage: pipeline.StageEngine,
		Agent: "engine-a", Attempt: 2, MaxAttempts: 2,
	})

	progress, ok := hub.current("doc-1")
	if !ok || len(progress.Agents) != 1 {
		t.Fatalf("progress = %+v, exists = %v", progress, ok)
	}
	agent := progress.Agents[0]
	if agent.Attempt != 2 || agent.MaxAttempts != 2 || agent.Output != "" ||
		agent.OutputChars != 0 || agent.EstimatedTokens != 0 || agent.FirstToken ||
		agent.LastError != "deadline exceeded" {
		t.Fatalf("retry progress = %+v", agent)
	}

	tracker.handle(pipeline.Event{
		Type: pipeline.EventDelta, Stage: pipeline.StageEngine,
		Agent: "engine-a", Kind: "output", Text: "成功文本",
	})
	tracker.handle(pipeline.Event{
		Type: pipeline.EventAgentDone, Stage: pipeline.StageEngine, Agent: "engine-a",
	})
	progress, _ = hub.current("doc-1")
	agent = progress.Agents[0]
	if agent.Status != "completed" || agent.Output != "成功文本" || strings.Contains(agent.Output, "失败尝试") {
		t.Fatalf("completed retry progress = %+v", agent)
	}
}

func TestDocumentProgressCountsThinkingAsFirstToken(t *testing.T) {
	hub := newDocumentProgressHub()
	tracker := hub.beginPage("doc-thinking", database.DocumentPage{ID: "page-1", PageNumber: 1}, 1)
	tracker.handle(pipeline.Event{
		Type: pipeline.EventAttemptStart, Stage: pipeline.StageEngine,
		Agent: "engine-a", Attempt: 1, MaxAttempts: 1,
	})
	time.Sleep(2 * time.Millisecond)
	tracker.handle(pipeline.Event{
		Type: pipeline.EventDelta, Stage: pipeline.StageEngine,
		Agent: "engine-a", Kind: "thinking", Text: "正在观察图片",
	})

	progress, ok := hub.current("doc-thinking")
	if !ok || len(progress.Agents) != 1 {
		t.Fatalf("progress = %+v, exists = %v", progress, ok)
	}
	agent := progress.Agents[0]
	if !agent.FirstToken || agent.TTFTMS <= 0 || agent.Status != "thinking" ||
		agent.Thinking != "正在观察图片" {
		t.Fatalf("thinking progress = %+v", agent)
	}
	if agent.OutputChars != 0 || agent.EstimatedTokens != 0 || agent.TPS != 0 ||
		agent.firstOutputAt != nil {
		t.Fatalf("thinking must not count as OCR output throughput: %+v", agent)
	}
}
