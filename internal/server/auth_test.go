package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lieyanc/BetterOCR/internal/config"
	"github.com/lieyanc/BetterOCR/internal/database"
)

func TestAuthenticationAndRoleGuards(t *testing.T) {
	cfg := serverConfig("http://127.0.0.1:1", "")
	store, err := database.Open(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitializeAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	reader, err := store.CreateUser("reader", "reader-password", database.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	h := (&Server{Config: cfg, Store: store}).Handler()

	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized config status = %d", unauthorized.Code)
	}

	adminCookie, adminCSRF := loginForTest(t, h, "admin", "admin-password")
	readerCookie, readerCSRF := loginForTest(t, h, "reader", "reader-password")
	readerAdmin := requestWithAuth(http.MethodGet, "/api/admin/users", nil, readerCookie, readerCSRF)
	readerAdminRec := httptest.NewRecorder()
	h.ServeHTTP(readerAdminRec, readerAdmin)
	if readerAdminRec.Code != http.StatusForbidden {
		t.Fatalf("reader admin status = %d body=%s", readerAdminRec.Code, readerAdminRec.Body)
	}
	selectionBody := bytes.NewReader([]byte(`{"engines":["test/tiny-a"],"arbiter":"","duplicate_checker":""}`))
	readerSelection := requestWithAuth(http.MethodPut, "/api/admin/model-selection", selectionBody, readerCookie, readerCSRF)
	readerSelection.Header.Set("Content-Type", "application/json")
	readerSelectionRec := httptest.NewRecorder()
	h.ServeHTTP(readerSelectionRec, readerSelection)
	if readerSelectionRec.Code != http.StatusForbidden {
		t.Fatalf("reader model selection status = %d body=%s", readerSelectionRec.Code, readerSelectionRec.Body)
	}

	createBody, _ := json.Marshal(map[string]any{"username": "second", "password": "second-password", "role": "user"})
	withoutCSRF := requestWithAuth(http.MethodPost, "/api/admin/users", bytes.NewReader(createBody), adminCookie, "")
	withoutCSRFRec := httptest.NewRecorder()
	h.ServeHTTP(withoutCSRFRec, withoutCSRF)
	if withoutCSRFRec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d", withoutCSRFRec.Code)
	}

	withCSRF := requestWithAuth(http.MethodPost, "/api/admin/users", bytes.NewReader(createBody), adminCookie, adminCSRF)
	withCSRF.Header.Set("Content-Type", "application/json")
	withCSRFRec := httptest.NewRecorder()
	h.ServeHTTP(withCSRFRec, withCSRF)
	if withCSRFRec.Code != http.StatusCreated {
		t.Fatalf("create user status = %d body=%s", withCSRFRec.Code, withCSRFRec.Body)
	}

	task, err := store.CreateTask(reader, "reader.png", []string{"test/tiny-a"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishTask(task.ID, nil, "upstream failed", 0); err != nil {
		t.Fatal(err)
	}
	readerTasks := requestWithAuth(http.MethodGet, "/api/tasks", nil, readerCookie, readerCSRF)
	readerTasksRec := httptest.NewRecorder()
	h.ServeHTTP(readerTasksRec, readerTasks)
	var tasks []database.Task
	if err := json.Unmarshal(readerTasksRec.Body.Bytes(), &tasks); err != nil || len(tasks) != 1 || tasks[0].UserID != reader.ID {
		t.Fatalf("reader tasks status=%d tasks=%+v err=%v", readerTasksRec.Code, tasks, err)
	}
	adminTasks := requestWithAuth(http.MethodGet, "/api/tasks", nil, adminCookie, adminCSRF)
	adminTasksRec := httptest.NewRecorder()
	h.ServeHTTP(adminTasksRec, adminTasks)
	if err := json.Unmarshal(adminTasksRec.Body.Bytes(), &tasks); err != nil || len(tasks) != 1 {
		t.Fatalf("admin tasks status=%d tasks=%+v err=%v", adminTasksRec.Code, tasks, err)
	}
}

func TestAuthenticatedOCRPersistsOwnedTask(t *testing.T) {
	upstream, _ := fakeUpstream("recorded text")
	defer upstream.Close()
	cfg := serverConfig(upstream.URL, "server-key")
	store, err := database.Open(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := (&Server{Config: cfg, Store: store}).Handler()
	cookie, csrf := setupForTest(t, h, "admin", "admin-password")
	body, contentType := multipartBody(t, testPNG(t), map[string]string{
		"engines": "test/tiny-a,test/tiny-b",
		"arbiter": "",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ocr", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("OCR status=%d body=%s", rec.Code, rec.Body)
	}
	tasks := store.Tasks("", true)
	if len(tasks) != 1 || tasks[0].Status != "completed" || tasks[0].Filename != "test.png" ||
		tasks[0].Result == nil || tasks[0].Result.Text != "recorded text" {
		t.Fatalf("persisted tasks = %+v", tasks)
	}
}

func TestWebSetupStatusAndSingleInitialization(t *testing.T) {
	cfg := serverConfig("http://127.0.0.1:1", "")
	store, err := database.Open(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := (&Server{Config: cfg, Store: store}).Handler()

	statusRec := httptest.NewRecorder()
	h.ServeHTTP(statusRec, httptest.NewRequest(http.MethodGet, "/api/setup/status", nil))
	if statusRec.Code != http.StatusOK || !bytes.Contains(statusRec.Body.Bytes(), []byte(`"initialized":false`)) {
		t.Fatalf("setup status=%d body=%s", statusRec.Code, statusRec.Body)
	}
	cookie, csrf := setupForTest(t, h, "owner", "owner-password")
	if cookie == nil || csrf == "" || !store.Initialized() {
		t.Fatalf("setup cookie=%v csrf=%q initialized=%v", cookie, csrf, store.Initialized())
	}

	body, _ := json.Marshal(map[string]string{"username": "second", "password": "second-password"})
	req := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second setup status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestAdminSettingsPersistToSharedConfigOnly(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	databasePath := filepath.Join(root, "database.json")
	cfg := serverConfig("http://127.0.0.1:1", "server-key")
	cfg.ServeAddr = "127.0.0.1:8787"
	cfg.EngineTimeoutSeconds = 30
	cfg.ArbiterTimeoutSeconds = 40
	cfg.EngineMaxAttempts = 2
	cfg.ArbiterMaxAttempts = 3
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := database.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Config: cfg, ConfigPath: configPath, Store: store}
	handler := srv.Handler()
	cookie, csrf := setupForTest(t, handler, "admin", "admin-password")

	next := cfg
	next.EngineTimeoutSeconds = 45
	next.ArbiterTimeoutSeconds = 60
	next.EngineMaxAttempts = 4
	next.ArbiterMaxAttempts = 5
	body, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	request := requestWithAuth(http.MethodPut, "/api/admin/settings", bytes.NewReader(body), cookie, csrf)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update settings status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	loaded, _, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	current := srv.currentConfig()
	if loaded.EngineTimeoutSeconds != 45 || loaded.ArbiterTimeoutSeconds != 60 ||
		loaded.EngineMaxAttempts != 4 || loaded.ArbiterMaxAttempts != 5 ||
		current.EngineTimeoutSeconds != 45 || current.ArbiterTimeoutSeconds != 60 {
		t.Fatalf("file settings=%+v server settings=%+v", loaded, current)
	}
	databaseJSON, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(databaseJSON, []byte(`"settings"`)) || bytes.Contains(databaseJSON, []byte("server-key")) {
		t.Fatalf("configuration leaked into database: %s", databaseJSON)
	}
}

func TestAdminModelSelectionPersistsWithoutOverwritingOtherSettings(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	cfg := serverConfig("http://127.0.0.1:1", "server-key")
	cfg.ServeAddr = "127.0.0.1:8787"
	cfg.EngineTimeoutSeconds = 37
	cfg.ArbiterTimeoutSeconds = 53
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	savedConfig, _, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.Open(filepath.Join(root, "database.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Config: savedConfig, ConfigPath: configPath, Store: store}
	handler := srv.Handler()
	cookie, csrf := setupForTest(t, handler, "admin", "admin-password")

	body := bytes.NewReader([]byte(`{"engines":["test/tiny-b"],"arbiter":"test/big","duplicate_checker":"test/tiny-a"}`))
	request := requestWithAuth(http.MethodPut, "/api/admin/model-selection", body, cookie, csrf)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update model selection status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "server-key") {
		t.Fatal("model selection response leaks the API key")
	}

	loaded, _, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	current := srv.currentConfig()
	for name, got := range map[string]config.Config{"file": loaded, "server": current} {
		if len(got.Engines) != 1 || got.Engines[0] != "test/tiny-b" ||
			got.Arbiter != "test/big" || got.DuplicateChecker != "test/tiny-a" {
			t.Errorf("%s model selection = %+v", name, got)
		}
		if got.Providers[0].APIKey != "server-key" || got.ServeAddr != "127.0.0.1:8787" ||
			got.EngineTimeoutSeconds != 37 || got.ArbiterTimeoutSeconds != 53 {
			t.Errorf("%s unrelated settings changed: %+v", name, got)
		}
	}

	badBody := bytes.NewReader([]byte(`{"engines":["test/missing"],"arbiter":"","duplicate_checker":""}`))
	badRequest := requestWithAuth(http.MethodPut, "/api/admin/model-selection", badBody, cookie, csrf)
	badRequest.Header.Set("Content-Type", "application/json")
	badRecorder := httptest.NewRecorder()
	handler.ServeHTTP(badRecorder, badRequest)
	if badRecorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid model selection status=%d body=%s", badRecorder.Code, badRecorder.Body.String())
	}
}

func loginForTest(t *testing.T, h http.Handler, username, password string) (*http.Cookie, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %q status=%d body=%s", username, rec.Code, rec.Body)
	}
	var response sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := rec.Result()
	cookies := result.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies = %v", cookies)
	}
	return cookies[0], response.CSRFToken
}

func setupForTest(t *testing.T, h http.Handler, username, password string) (*http.Cookie, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status=%d body=%s", rec.Code, rec.Body)
	}
	var response sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("setup cookies = %v", cookies)
	}
	return cookies[0], response.CSRFToken
}

func requestWithAuth(method, target string, body *bytes.Reader, cookie *http.Cookie, csrf string) *http.Request {
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, body)
	}
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	return req
}
