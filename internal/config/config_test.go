package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lieyanc/BetterOCR/internal/model"
)

func localProvider(baseURL, apiKey string) model.Provider {
	return model.Provider{
		ID: "local", BaseURL: baseURL, APIKey: apiKey,
		Models: []model.Definition{{
			ID: "vision", Context: 32768, Alias: "Local Vision", API: model.APIOpenAIChat,
		}},
	}
}

func TestLoadReleasesTemplateWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "betterocr.json")
	cfg, action, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionReleased {
		t.Errorf("action = %v, want ActionReleased", action)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("cfg = %+v, want Default()", cfg)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("模板未落盘:", err)
	}
	var round Config
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal("落盘模板不是合法 JSON:", err)
	}
	if !reflect.DeepEqual(round, Default()) {
		t.Errorf("落盘模板 = %+v, want Default()", round)
	}
}

func TestLoadTreatsEmptyFileAsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "betterocr.json")
	if err := os.WriteFile(path, []byte(" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, action, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionReleased {
		t.Errorf("action = %v, want ActionReleased", action)
	}
}

func TestLoadSupplementsMissingFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "betterocr.json")
	partial := Config{
		Providers: []model.Provider{localProvider("http://localhost:11434/v1", "sk-x")},
		Engines:   []string{"local/vision"},
		Arbiter:   "",
	}
	raw, _ := json.Marshal(map[string]any{
		"providers": partial.Providers,
		"engines":   partial.Engines,
		"arbiter":   partial.Arbiter,
	})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, action, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionSupplemented {
		t.Errorf("action = %v, want ActionSupplemented", action)
	}
	if !reflect.DeepEqual(cfg.Providers, partial.Providers) || !reflect.DeepEqual(cfg.Engines, partial.Engines) {
		t.Errorf("用户字段被改写: %+v", cfg)
	}
	if cfg.Arbiter != "" {
		t.Errorf("显式空 arbiter 被改写: %q", cfg.Arbiter)
	}
	if cfg.TimeoutSeconds != Default().TimeoutSeconds || cfg.ServeAddr != Default().ServeAddr {
		t.Errorf("缺失字段未按模板补全: %+v", cfg)
	}
	_, action, err = Load(path)
	if err != nil || action != ActionNone {
		t.Errorf("补全后二次加载 = (%v, %v), want ActionNone", action, err)
	}
}

func TestLoadMigratesLegacyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "betterocr.json")
	legacy := []byte(`{"engines":["small","small"],"arbiter":"large","base_url":"http://legacy/v1","api_key":"secret","timeout_seconds":30,"serve_addr":":1"}`)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, action, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionMigrated {
		t.Fatalf("action = %v, want ActionMigrated", action)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].ID != "default" || cfg.Providers[0].APIKey != "secret" {
		t.Fatalf("provider = %+v", cfg.Providers)
	}
	if got := cfg.Engines; !reflect.DeepEqual(got, []string{"default/small", "default/small"}) {
		t.Errorf("engines = %v", got)
	}
	if cfg.Arbiter != "default/large" || len(cfg.Providers[0].Models) != 2 {
		t.Errorf("migrated config = %+v", cfg)
	}
	written, _ := os.ReadFile(path)
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(written, &topLevel); err != nil {
		t.Fatal(err)
	}
	if _, ok := topLevel["providers"]; !ok {
		t.Errorf("migrated config has no providers: %s", written)
	}
	if _, ok := topLevel["api_key"]; ok {
		t.Errorf("legacy top-level api_key remains: %s", written)
	}
	if _, action, err := Load(path); err != nil || action != ActionNone {
		t.Errorf("migrated reload = (%v, %v)", action, err)
	}
}

func TestLoadKeepsCompleteFileByteForByte(t *testing.T) {
	path := filepath.Join(t.TempDir(), "betterocr.json")
	full := []byte(`{"providers":[{"id":"local","base_url":"http://b","api_key":"","models":[{"id":"m","context":4096,"alias":"M","api":"anthropic"}]}],"engines":["local/m"],"arbiter":"","timeout_seconds":30,"serve_addr":":1"}`)
	if err := os.WriteFile(path, full, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, action, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionNone || cfg.Arbiter != "" || cfg.TimeoutSeconds != 30 {
		t.Errorf("load = (%+v, %v)", cfg, action)
	}
	raw, _ := os.ReadFile(path)
	if !bytes.Equal(raw, full) {
		t.Errorf("字段齐全仍被重写:\n%s", raw)
	}
}

func TestLoadNeverRewritesInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "betterocr.json")
	for _, orig := range [][]byte{
		[]byte("{broken json"),
		[]byte(`{"providers":[],"engines":["missing/model"],"arbiter":"","timeout_seconds":30,"serve_addr":":1"}`),
	} {
		if err := os.WriteFile(path, orig, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Load(path); err == nil {
			t.Fatalf("invalid config %q should fail", orig)
		}
		raw, _ := os.ReadFile(path)
		if !bytes.Equal(raw, orig) {
			t.Errorf("invalid file was modified:\n%s", raw)
		}
	}
}

func TestWriteTightensExistingFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "betterocr.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := write(path, Default()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
}

func TestValidateAndResolve(t *testing.T) {
	cfg := Config{
		Providers: []model.Provider{localProvider("http://local/v1", "key")},
		Engines:   []string{"local/vision"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	resolved, err := cfg.Resolve("local/vision")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.APIKey != "key" || resolved.API != model.APIOpenAIChat || resolved.Context != 32768 {
		t.Errorf("resolved = %+v", resolved)
	}

	bad := cfg
	bad.Providers = append([]model.Provider(nil), cfg.Providers...)
	bad.Providers[0].Models = append([]model.Definition(nil), cfg.Providers[0].Models...)
	bad.Providers[0].Models[0].API = "unknown"
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "不受支持") {
		t.Errorf("invalid api error = %v", err)
	}
}

func TestTimeout(t *testing.T) {
	if got := (Config{TimeoutSeconds: 30}).Timeout(); got != 30*time.Second {
		t.Errorf("Timeout(30) = %v", got)
	}
	if got := (Config{}).Timeout(); got != 2*time.Minute {
		t.Errorf("Timeout(0) = %v, want 2m", got)
	}
}

func TestFieldKeysCoverAllFields(t *testing.T) {
	keys := fieldKeys()
	n := reflect.TypeOf(Config{}).NumField()
	if len(keys) != n {
		t.Errorf("fieldKeys 覆盖 %d 个字段,结构体有 %d 个", len(keys), n)
	}
}
