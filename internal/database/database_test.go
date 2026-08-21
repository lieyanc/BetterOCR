package database

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/lieyanc/BetterOCR/internal/arbiter"
	"github.com/lieyanc/BetterOCR/internal/config"
)

func TestStoreLifecycleAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database.json")
	store, err := Open(path, config.Default())
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

	task, err := store.CreateTask(user, "invoice.png", []string{"openai/gpt-4o-mini"}, "")
	if err != nil {
		t.Fatal(err)
	}
	final := arbiter.Final{Text: "识别结果"}
	if err := store.FinishTask(task.ID, &final, "", 1250*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	tasks := store.Tasks(user.ID, false)
	if len(tasks) != 1 || tasks[0].Status != "completed" || tasks[0].Result == nil || tasks[0].Result.Text != "识别结果" {
		t.Fatalf("tasks = %+v", tasks)
	}
	if tasks := store.Tasks(admin.ID, false); len(tasks) != 0 {
		t.Fatalf("admin-owned tasks = %+v, want none", tasks)
	}
	if tasks := store.Tasks(admin.ID, true); len(tasks) != 1 {
		t.Fatalf("all tasks = %+v", tasks)
	}
	invalidSettings := store.Settings()
	invalidSettings.ServeAddr = ""
	if err := store.UpdateSettings(invalidSettings); err == nil {
		t.Fatal("empty serve_addr was accepted")
	}

	if err := store.Logout(token); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := store.Authenticate(token); ok {
		t.Fatal("logged-out session still authenticates")
	}
	reopened, err := Open(path, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Initialized() || len(reopened.Users()) != 2 || len(reopened.Tasks(user.ID, false)) != 1 {
		t.Fatalf("reopened initialized=%v users=%d tasks=%d", reopened.Initialized(), len(reopened.Users()), len(reopened.Tasks(user.ID, false)))
	}
}

func TestInitializeAdminAllowsOneConcurrentWinner(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "database.json"), config.Default())
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
	store, err := Open(path, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	admin, err := store.InitializeAdmin("admin", "admin-password")
	if err != nil {
		t.Fatal(err)
	}
	processing, err := store.CreateDocument(admin, "processing.pdf", "pdf", "application/pdf", 1024, []string{"openai/gpt-4o-mini"}, "", false)
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
	preparing, err := store.CreateDocument(admin, "preparing.pdf", "pdf", "application/pdf", 2048, []string{"openai/gpt-4o-mini"}, "", false)
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, config.Default())
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
	if !ok || recovered.Status != DocumentReady || recovered.Pages[0].Status != PageCompleted || recovered.Pages[1].Status != PageQueued {
		t.Fatalf("recovered = %+v ok=%v", recovered, ok)
	}
}
