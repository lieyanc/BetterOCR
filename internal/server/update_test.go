package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lieyanc/BetterOCR/internal/database"
	"github.com/lieyanc/BetterOCR/internal/updater"
	"github.com/lieyanc/BetterOCR/internal/version"
)

func TestVersionEndpointIsPublicAndReportsUpdateSettings(t *testing.T) {
	cfg := serverConfig("http://127.0.0.1:1", "")
	cfg.Update = updater.Config{
		Enabled: true, Channel: "dev", Source: "proxy",
		ProxyBaseURL: "https://mirror.example/", Repo: "owner/repo",
	}
	store, err := database.Open(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := (&Server{Config: cfg, Store: store}).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("version status = %d body=%s", rec.Code, rec.Body)
	}
	var got versionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != version.Version || got.Commit != version.Commit || got.BuildTime != version.BuildTime {
		t.Errorf("build identity = %+v", got)
	}
	if !got.UpdateEnabled || got.UpdateChannel != "dev" || got.UpdateSource != "proxy" || got.UpdateRepo != "owner/repo" {
		t.Errorf("update settings = %+v", got)
	}
}

// TestUpdateEndpointsRequireAdminSession covers the whole guard matrix: the
// update actions change server state, so an anonymous or non-admin caller must
// never reach them.
func TestUpdateEndpointsRequireAdminSession(t *testing.T) {
	cfg := serverConfig("http://127.0.0.1:1", "")
	store, err := database.Open(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitializeAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser("reader", "reader-password", database.RoleUser); err != nil {
		t.Fatal(err)
	}
	// Updater 为 nil:鉴权必须先于"未启用自更新"生效,否则 503 就成了探测口。
	h := (&Server{Config: cfg, Store: store}).Handler()
	adminCookie, adminCSRF := loginForTest(t, h, "admin", "admin-password")
	readerCookie, readerCSRF := loginForTest(t, h, "reader", "reader-password")

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/update/status"},
		{http.MethodPost, "/api/update/check"},
		{http.MethodPost, "/api/update/apply"},
		{http.MethodPost, "/api/update/dismiss"},
	}
	for _, endpoint := range endpoints {
		anonymous := httptest.NewRecorder()
		h.ServeHTTP(anonymous, httptest.NewRequest(endpoint.method, endpoint.path, nil))
		if anonymous.Code != http.StatusUnauthorized {
			t.Errorf("%s %s anonymous = %d, want 401", endpoint.method, endpoint.path, anonymous.Code)
		}

		reader := httptest.NewRecorder()
		h.ServeHTTP(reader, requestWithAuth(endpoint.method, endpoint.path, nil, readerCookie, readerCSRF))
		if reader.Code != http.StatusForbidden {
			t.Errorf("%s %s reader = %d, want 403", endpoint.method, endpoint.path, reader.Code)
		}

		if endpoint.method != http.MethodGet {
			noCSRF := httptest.NewRecorder()
			h.ServeHTTP(noCSRF, requestWithAuth(endpoint.method, endpoint.path, nil, adminCookie, ""))
			if noCSRF.Code != http.StatusForbidden {
				t.Errorf("%s %s without CSRF = %d, want 403", endpoint.method, endpoint.path, noCSRF.Code)
			}
		}

		admin := httptest.NewRecorder()
		h.ServeHTTP(admin, requestWithAuth(endpoint.method, endpoint.path, nil, adminCookie, adminCSRF))
		if admin.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s admin without updater = %d, want 503", endpoint.method, endpoint.path, admin.Code)
		}
	}
}

func TestUpdateStatusAndCheckThroughProxyMirror(t *testing.T) {
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/releases/owner/repo/latest" {
			t.Errorf("unexpected mirror path: %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","body":"fixture notes","assets":[]}`))
	}))
	defer mirror.Close()

	cfg := serverConfig("http://127.0.0.1:1", "")
	cfg.Update = updater.Config{
		Enabled: true, Channel: "stable", Source: "proxy",
		ProxyBaseURL: mirror.URL, Repo: "owner/repo",
	}
	store, err := database.Open(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Config: cfg, Store: store}
	dataDir := t.TempDir()
	srv.Updater = updater.New(
		srv.UpdateConfig,
		func() string { return dataDir },
		log.New(io.Discard, "", 0),
		updater.RestartHooks{},
	)
	h := srv.Handler()
	cookie, csrf := setupForTest(t, h, "admin", "admin-password")

	statusRec := httptest.NewRecorder()
	h.ServeHTTP(statusRec, requestWithAuth(http.MethodGet, "/api/update/status", nil, cookie, csrf))
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", statusRec.Code, statusRec.Body)
	}
	var status updater.Status
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.State != "idle" || status.CurrentVersion != version.Version {
		t.Fatalf("status = %+v", status)
	}

	checkRec := httptest.NewRecorder()
	h.ServeHTTP(checkRec, requestWithAuth(http.MethodPost, "/api/update/check", bytes.NewReader(nil), cookie, csrf))
	if checkRec.Code != http.StatusOK {
		t.Fatalf("check = %d body=%s", checkRec.Code, checkRec.Body)
	}
	var result updateCheckResponse
	if err := json.Unmarshal(checkRec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	// 测试二进制的 version.Version 是 "dev",按设计永远视为可更新。
	if result.Error != "" || !result.HasUpdate || result.LatestVersion != "v9.9.9" || result.Channel != "stable" {
		t.Fatalf("check result = %+v", result)
	}
	if result.ReleaseNotes != "fixture notes" {
		t.Errorf("release notes = %q", result.ReleaseNotes)
	}
}

func TestUpdateCheckReportsFailureInBody(t *testing.T) {
	cfg := serverConfig("http://127.0.0.1:1", "")
	// 指向一个没人监听的端口,检查必然失败。
	cfg.Update = updater.Config{Enabled: true, Source: "proxy", ProxyBaseURL: "http://127.0.0.1:1", Repo: "owner/repo"}
	store, err := database.Open(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Config: cfg, Store: store}
	srv.Updater = updater.New(srv.UpdateConfig, t.TempDir, log.New(io.Discard, "", 0), updater.RestartHooks{})
	h := srv.Handler()
	cookie, csrf := setupForTest(t, h, "admin", "admin-password")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestWithAuth(http.MethodPost, "/api/update/check", bytes.NewReader(nil), cookie, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("check status = %d body=%s", rec.Code, rec.Body)
	}
	var result updateCheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error == "" || result.HasUpdate {
		t.Fatalf("check result = %+v, want an inline error", result)
	}
}

func TestUpdateConfigNormalizesLiveSettings(t *testing.T) {
	cfg := serverConfig("http://127.0.0.1:1", "")
	cfg.Update = updater.Config{Channel: "  ", Source: "weird", Repo: ""}
	srv := &Server{Config: cfg}

	got := srv.UpdateConfig()
	if got.Channel != "stable" || got.Source != updater.SourceGitHub || got.Repo != updater.DefaultRepo {
		t.Fatalf("UpdateConfig = %+v", got)
	}

	// 管理员保存设置后 Config 被替换,updater 每次取值都要看到新值。
	srv.configMu.Lock()
	srv.Config.Update.Channel = "dev"
	srv.configMu.Unlock()
	if channel := srv.UpdateConfig().Channel; channel != "dev" {
		t.Fatalf("channel after settings change = %q, want dev", channel)
	}
}

// TestHasActiveJobsWhileRecognitionRuns guards the pairing the updater depends
// on: a restart must not land in the middle of a recognition.
func TestHasActiveJobsWhileRecognitionRuns(t *testing.T) {
	var once sync.Once
	started := make(chan struct{})
	unblock := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(started) })
		<-unblock
		body, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "held text"}}},
		})
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	srv := &Server{Config: serverConfig(upstream.URL, "")}
	h := srv.Handler()
	if srv.HasActiveJobs() {
		t.Fatal("idle server reports active jobs")
	}

	done := make(chan int, 1)
	go func() {
		rec := postOCR(t, h, testPNG(t), map[string]string{"engines": "test/tiny-a", "arbiter": ""})
		done <- rec.Code
	}()

	<-started
	if !srv.HasActiveJobs() {
		t.Error("in-flight recognition not reported as active")
	}
	close(unblock)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("OCR status = %d", code)
	}
	if srv.HasActiveJobs() {
		t.Error("finished recognition still reported as active")
	}
}

func TestDocumentManagerReportsQueuedAndRunningJobs(t *testing.T) {
	manager := &documentManager{queued: map[string]bool{}, runs: map[string]context.CancelFunc{}}
	if manager.active() {
		t.Fatal("empty manager reports active jobs")
	}
	manager.queued["process:doc-1"] = true
	if !manager.active() {
		t.Error("queued job not reported as active")
	}
	delete(manager.queued, "process:doc-1")
	manager.runs["doc-1"] = func() {}
	if !manager.active() {
		t.Error("running job not reported as active")
	}
}
