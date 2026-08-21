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
	"testing"
	"time"

	"github.com/lieyanc/BetterOCR/internal/arbiter"
	"github.com/lieyanc/BetterOCR/internal/database"
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
