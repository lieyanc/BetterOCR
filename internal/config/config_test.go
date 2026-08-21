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
			ID: "vision", Context: 32768, Alias: "Local Vision", API: model.APIOpenAIChatCompletions,
		}},
	}
}

func TestLoadReleasesTemplateWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "config.json")
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
	// alias 为空会被补全为 id(local 不在模板中,回退 id 本身)
	want := append([]model.Provider(nil), partial.Providers...)
	want[0].Alias = "local"
	if !reflect.DeepEqual(cfg.Providers, want) || !reflect.DeepEqual(cfg.Engines, partial.Engines) {
		t.Errorf("用户字段被改写: %+v", cfg)
	}
	if cfg.Arbiter != "" {
		t.Errorf("显式空 arbiter 被改写: %q", cfg.Arbiter)
	}
	if cfg.EngineTimeoutSeconds != Default().EngineTimeoutSeconds ||
		cfg.ArbiterTimeoutSeconds != Default().ArbiterTimeoutSeconds ||
		cfg.EngineMaxAttempts != Default().EngineMaxAttempts ||
		cfg.ArbiterMaxAttempts != Default().ArbiterMaxAttempts ||
		cfg.ServeAddr != Default().ServeAddr {
		t.Errorf("缺失字段未按模板补全: %+v", cfg)
	}
	_, action, err = Load(path)
	if err != nil || action != ActionNone {
		t.Errorf("补全后二次加载 = (%v, %v), want ActionNone", action, err)
	}
}

func TestLoadSupplementsProviderAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "betterocr.json")
	// 模板同 id(openai)沿用模板 alias;未知 id 回退 id 本身
	full := []byte(`{"providers":[{"id":"openai","base_url":"http://o/v1","api_key":"","models":[{"id":"m","context":4096,"alias":"M","api":"openai-responses"}]},{"id":"custom","base_url":"http://c/v1","api_key":"","models":[{"id":"n","context":4096,"alias":"N","api":"openai-responses"}]}],"engines":["openai/m"],"arbiter":"","timeout_seconds":30,"serve_addr":":1"}`)
	if err := os.WriteFile(path, full, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, action, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionSupplemented {
		t.Fatalf("action = %v, want ActionSupplemented", action)
	}
	if cfg.Providers[0].Alias != "OpenAI" {
		t.Errorf("模板同 id 的 alias = %q, want OpenAI", cfg.Providers[0].Alias)
	}
	if cfg.Providers[1].Alias != "custom" {
		t.Errorf("未知 id 的 alias = %q, want custom", cfg.Providers[1].Alias)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"alias": "OpenAI"`) {
		t.Errorf("补全的 alias 未落盘:\n%s", raw)
	}
	// 二次加载:alias 已齐全,不再写回
	if _, action, err := Load(path); err != nil || action != ActionNone {
		t.Errorf("补全后二次加载 = (%v, %v), want ActionNone", action, err)
	}
}

func TestLoadKeepsExplicitProviderAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "betterocr.json")
	full := []byte(`{"providers":[{"id":"openai","alias":"My OpenAI","base_url":"http://o/v1","api_key":"","models":[{"id":"m","context":4096,"alias":"M","api":"openai-responses"}]}],"engines":["openai/m"],"arbiter":"","engine_timeout_seconds":30,"arbiter_timeout_seconds":45,"engine_max_attempts":2,"arbiter_max_attempts":3,"serve_addr":":1"}`)
	if err := os.WriteFile(path, full, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, action, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionNone {
		t.Errorf("显式 alias 触发写回: action = %v", action)
	}
	if cfg.Providers[0].Alias != "My OpenAI" {
		t.Errorf("显式 alias 被改写: %q", cfg.Providers[0].Alias)
	}
}

func TestLoadKeepsCompleteFileByteForByte(t *testing.T) {
	path := filepath.Join(t.TempDir(), "betterocr.json")
	// 顶层字段与 provider alias 均齐全 → 原样使用,逐字节不变
	full := []byte(`{"providers":[{"id":"local","alias":"Local","base_url":"http://b","api_key":"","models":[{"id":"m","context":4096,"alias":"M","api":"anthropic-messages"}]}],"engines":["local/m"],"arbiter":"","engine_timeout_seconds":30,"arbiter_timeout_seconds":45,"engine_max_attempts":2,"arbiter_max_attempts":3,"serve_addr":":1"}`)
	if err := os.WriteFile(path, full, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, action, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionNone || cfg.Arbiter != "" || cfg.EngineTimeoutSeconds != 30 || cfg.ArbiterTimeoutSeconds != 45 {
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

func TestSaveRejectsInvalidConfigWithoutReplacingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := Default()
	if err := Save(path, original); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid := original
	invalid.Engines = []string{"missing/model"}
	if err := Save(path, invalid); err == nil {
		t.Fatal("invalid configuration was saved")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("invalid save replaced the existing configuration")
	}
}

func TestValidateRejectsInvalidStagePolicies(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"negative engine timeout":   func(cfg *Config) { cfg.EngineTimeoutSeconds = -1 },
		"negative arbiter timeout":  func(cfg *Config) { cfg.ArbiterTimeoutSeconds = -1 },
		"too many engine attempts":  func(cfg *Config) { cfg.EngineMaxAttempts = 11 },
		"negative arbiter attempts": func(cfg *Config) { cfg.ArbiterMaxAttempts = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", cfg)
			}
		})
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
	if resolved.APIKey != "key" || resolved.API != model.APIOpenAIChatCompletions || resolved.Context != 32768 {
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

func TestStageTimeoutsAndAttempts(t *testing.T) {
	cfg := Config{
		EngineTimeoutSeconds: 30, ArbiterTimeoutSeconds: 45,
		EngineMaxAttempts: 3, ArbiterMaxAttempts: 4,
	}
	if got := cfg.EngineTimeout(); got != 30*time.Second {
		t.Errorf("EngineTimeout = %v", got)
	}
	if got := cfg.ArbiterTimeout(); got != 45*time.Second {
		t.Errorf("ArbiterTimeout = %v", got)
	}
	if cfg.EngineAttempts() != 3 || cfg.ArbiterAttempts() != 4 {
		t.Errorf("attempts = (%d, %d)", cfg.EngineAttempts(), cfg.ArbiterAttempts())
	}
	legacy := Config{TimeoutSeconds: 77}
	if legacy.EngineTimeout() != 77*time.Second || legacy.ArbiterTimeout() != 77*time.Second {
		t.Errorf("legacy stage timeouts = (%v, %v)", legacy.EngineTimeout(), legacy.ArbiterTimeout())
	}
}

func TestLoadMigratesLegacyTimeoutIntoIndependentStages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := []byte(`{"providers":[{"id":"local","alias":"Local","base_url":"http://b","api_key":"","models":[{"id":"m","context":4096,"alias":"M","api":"anthropic-messages"}]}],"engines":["local/m"],"arbiter":"","timeout_seconds":77,"serve_addr":":1"}`)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, action, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionSupplemented || cfg.EngineTimeoutSeconds != 77 || cfg.ArbiterTimeoutSeconds != 77 {
		t.Fatalf("migration = (%+v, %v)", cfg, action)
	}
	if cfg.EngineMaxAttempts != 2 || cfg.ArbiterMaxAttempts != 2 {
		t.Fatalf("migrated attempts = (%d, %d)", cfg.EngineMaxAttempts, cfg.ArbiterMaxAttempts)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"timeout_seconds"`)) ||
		!bytes.Contains(raw, []byte(`"engine_timeout_seconds": 77`)) ||
		!bytes.Contains(raw, []byte(`"arbiter_timeout_seconds": 77`)) {
		t.Fatalf("legacy timeout was not normalized: %s", raw)
	}
}

func TestFieldKeysCoverAllFields(t *testing.T) {
	keys := fieldKeys()
	n := reflect.TypeOf(Config{}).NumField() - 1 // TimeoutSeconds is migration-only.
	if len(keys) != n {
		t.Errorf("fieldKeys 覆盖 %d 个字段,结构体有 %d 个", len(keys), n)
	}
}
