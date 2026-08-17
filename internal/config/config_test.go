package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

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
	if err := os.WriteFile(path, []byte(`{"engines":["my-model"],"api_key":"sk-x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, action, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionSupplemented {
		t.Errorf("action = %v, want ActionSupplemented", action)
	}
	// 文件里出现的字段以文件为准
	if !reflect.DeepEqual(cfg.Engines, []string{"my-model"}) || cfg.APIKey != "sk-x" {
		t.Errorf("用户字段被改写: %+v", cfg)
	}
	// 缺失字段用模板补齐
	def := Default()
	if cfg.Arbiter != def.Arbiter || cfg.BaseURL != def.BaseURL ||
		cfg.TimeoutSeconds != def.TimeoutSeconds || cfg.ServeAddr != def.ServeAddr {
		t.Errorf("缺失字段未按模板补全: %+v", cfg)
	}
	// 补全已写回:再次加载应字段齐全、不再变动
	cfg2, action2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if action2 != ActionNone {
		t.Errorf("补全写回后 action = %v, want ActionNone", action2)
	}
	if !reflect.DeepEqual(cfg2, cfg) {
		t.Errorf("二次加载结果漂移: %+v != %+v", cfg2, cfg)
	}
}

func TestLoadKeepsExplicitEmptyValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "betterocr.json")
	// 字段齐全但 arbiter 显式置空(用户主动关闭仲裁),不得被模板改写
	full := []byte(`{"engines":["m"],"arbiter":"","base_url":"http://b","api_key":"","timeout_seconds":30,"serve_addr":":1"}`)
	if err := os.WriteFile(path, full, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, action, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionNone {
		t.Errorf("action = %v, want ActionNone", action)
	}
	if cfg.Arbiter != "" || cfg.APIKey != "" || cfg.TimeoutSeconds != 30 {
		t.Errorf("显式空值/用户值被改写: %+v", cfg)
	}
	// 字段齐全时文件应原封不动(保留用户自己的格式)
	raw, _ := os.ReadFile(path)
	if !bytes.Equal(raw, full) {
		t.Errorf("字段齐全仍被重写:\n%s", raw)
	}
}

func TestLoadNeverRewritesUnparsableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "betterocr.json")
	orig := []byte("{broken json")
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil {
		t.Fatal("解析失败应报错")
	}
	raw, _ := os.ReadFile(path)
	if !bytes.Equal(raw, orig) {
		t.Errorf("解析失败时文件被改动:\n%s", raw)
	}
}

func TestTimeout(t *testing.T) {
	if got := (Config{TimeoutSeconds: 30}).Timeout(); got != 30*time.Second {
		t.Errorf("Timeout(30) = %v", got)
	}
	if got := (Config{}).Timeout(); got != 2*time.Minute {
		t.Errorf("Timeout(0) = %v, want 2m", got)
	}
	if got := (Config{TimeoutSeconds: -5}).Timeout(); got != 2*time.Minute {
		t.Errorf("Timeout(-5) = %v, want 2m", got)
	}
}

func TestFieldKeysCoverAllFields(t *testing.T) {
	keys := fieldKeys()
	n := reflect.TypeOf(Config{}).NumField()
	if len(keys) != n {
		t.Errorf("fieldKeys 覆盖 %d 个字段,结构体有 %d 个", len(keys), n)
	}
}
