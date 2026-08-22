package database

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/lieyanc/BetterOCR/internal/arbiter"
)

func TestStoreLifecycleAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if store.Initialized() {
		t.Fatal("new database is already initialized")
	}
	if _, _, _, err := store.Login("admin", "admin-password"); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("login before setup error = %v", err)
	}
	if _, err := store.InitializeAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitializeAdmin("other-admin", "other-password"); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second initialization error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	if _, _, _, err := store.Login("admin", "wrong-password"); err == nil {
		t.Fatal("wrong password unexpectedly succeeded")
	}
	admin, token, csrf, err := store.Login("ADMIN", "admin-password")
	if err != nil {
		t.Fatal(err)
	}
	if admin.Role != RoleAdmin || token == "" || csrf == "" {
		t.Fatalf("login = %+v token=%q csrf=%q", admin, token, csrf)
	}
	got, gotCSRF, ok := store.Authenticate(token)
	if !ok || got.ID != admin.ID || gotCSRF != csrf {
		t.Fatalf("authenticate = %+v %q %v", got, gotCSRF, ok)
	}

	user, err := store.CreateUser("reader", "reader-password", RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser("READER", "reader-password", RoleUser); err == nil {
		t.Fatal("duplicate case-insensitive username unexpectedly succeeded")
	}
	disabled := true
	if _, err := store.UpdateUser(admin.ID, UserUpdate{Disabled: &disabled}); err == nil {
		t.Fatal("last administrator was disabled")
	}
	if err := store.DeleteUser(admin.ID); err == nil {
		t.Fatal("last administrator was deleted")
	}

	task, err := store.CreateTask(user, "invoice.png", []string{"openai/gpt-4o-mini"}, "", "openai/quick")
	if err != nil {
		t.Fatal(err)
	}
	final := arbiter.Final{Text: "识别结果"}
	if err := store.FinishTask(task.ID, &final, "", 1250*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	tasks := store.Tasks(user.ID, false)
	if len(tasks) != 1 || tasks[0].Status != "completed" || tasks[0].Result == nil ||
		tasks[0].Result.Text != "识别结果" || tasks[0].DuplicateChecker != "openai/quick" {
		t.Fatalf("tasks = %+v", tasks)
	}
	if tasks := store.Tasks(admin.ID, false); len(tasks) != 0 {
		t.Fatalf("admin-owned tasks = %+v, want none", tasks)
	}
	if tasks := store.Tasks(admin.ID, true); len(tasks) != 1 {
		t.Fatalf("all tasks = %+v", tasks)
	}
	if err := store.Logout(token); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := store.Authenticate(token); ok {
		t.Fatal("logged-out session still authenticates")
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Initialized() || len(reopened.Users()) != 2 || len(reopened.Tasks(user.ID, false)) != 1 {
		t.Fatalf("reopened initialized=%v users=%d tasks=%d", reopened.Initialized(), len(reopened.Users()), len(reopened.Tasks(user.ID, false)))
	}
}

func TestInitializeAdminAllowsOneConcurrentWinner(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, username := range []string{"admin-one", "admin-two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.InitializeAdmin(username, "admin-password")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	succeeded := 0
	alreadyInitialized := 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrAlreadyInitialized):
			alreadyInitialized++
		default:
			t.Fatalf("unexpected initialization error: %v", err)
		}
	}
	if succeeded != 1 || alreadyInitialized != 1 || len(store.Users()) != 1 {
		t.Fatalf("succeeded=%d already_initialized=%d users=%d", succeeded, alreadyInitialized, len(store.Users()))
	}
}

func TestRecoverDocumentsQueuesInterruptedWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := store.InitializeAdmin("admin", "admin-password")
	if err != nil {
		t.Fatal(err)
	}
	processing, err := store.CreateDocument(admin, "processing.pdf", "pdf", "application/pdf", 1024, []string{"openai/gpt-4o-mini"}, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MutateDocument(processing.ID, func(document *DocumentProject) error {
		document.Status = DocumentProcessing
		document.Pages = []DocumentPage{
			{ID: "page-000001", SourcePage: 1, Status: PageCompleted, ImageReady: true, ResultReady: true},
			{ID: "page-000002", SourcePage: 2, Status: PageProcessing, ImageReady: true},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	preparing, err := store.CreateDocument(admin, "preparing.pdf", "pdf", "application/pdf", 2048, []string{"openai/gpt-4o-mini"}, "", "", false)
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := reopened.RecoverDocuments()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(recovery.Process, processing.ID) || !slices.Contains(recovery.Prepare, preparing.ID) {
		t.Fatalf("recovery = %+v", recovery)
	}
	recovered, ok := reopened.Document(processing.ID)
	if !ok || recovered.Status != DocumentProcessing || recovered.Pages[0].Status != PageCompleted || recovered.Pages[1].Status != PageQueued {
		t.Fatalf("recovered = %+v ok=%v", recovered, ok)
	}
}

func TestOpenMigratesIdleQueuedPagesToReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := store.InitializeAdmin("admin", "admin-password")
	if err != nil {
		t.Fatal(err)
	}
	idle, err := store.CreateDocument(admin, "idle.pdf", "pdf", "application/pdf", 1, []string{"openai/test"}, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MutateDocument(idle.ID, func(document *DocumentProject) error {
		document.Status = DocumentReady
		document.Pages = []DocumentPage{{ID: "page-000001", Status: PageQueued, ImageReady: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy data
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.Version = 2
	raw, err = json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	document, ok := migrated.Document(idle.ID)
	if !ok || document.Pages[0].Status != PageReady {
		t.Fatalf("migrated document = %+v, ok=%v", document, ok)
	}
}

func TestFailInterruptedTasksClosesRunningRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := store.InitializeAdmin("admin", "admin-password")
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := store.CreateTask(admin, "interrupted.png", []string{"openai/gpt-4o-mini"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	finished, err := store.CreateTask(admin, "finished.png", []string{"openai/gpt-4o-mini"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishTask(finished.ID, &arbiter.Final{Text: "done"}, "", time.Second); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := reopened.FailInterruptedTasks()
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}
	byID := map[string]Task{}
	for _, task := range reopened.Tasks("", true) {
		byID[task.ID] = task
	}
	if got := byID[interrupted.ID]; got.Status != "failed" || got.Error == "" || got.CompletedAt == nil {
		t.Errorf("interrupted task = %+v", got)
	}
	if got := byID[finished.ID]; got.Status != "completed" || got.Error != "" {
		t.Errorf("completed task was rewritten: %+v", got)
	}
	// 幂等:再次启动不应重复改写。
	if again, err := reopened.FailInterruptedTasks(); err != nil || again != 0 {
		t.Fatalf("second recovery = (%d, %v), want (0, nil)", again, err)
	}
}

func TestDatabaseNeverPersistsSettingsAndRemovesLegacyCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"settings"`)) {
		t.Fatalf("new database contains settings: %s", raw)
	}

	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy["version"] = json.RawMessage("1")
	legacy["settings"] = json.RawMessage(`{"serve_addr":"127.0.0.1:8787"}`)
	raw, err = json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.LegacySettings()) == 0 {
		t.Fatal("legacy settings were not exposed for config migration")
	}
	if err := store.DiscardLegacySettings(); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"settings"`)) {
		t.Fatalf("legacy settings remain in database: %s", raw)
	}
	var migrated struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &migrated); err != nil || migrated.Version != databaseVersion {
		t.Fatalf("migrated database version=%d err=%v", migrated.Version, err)
	}
}
