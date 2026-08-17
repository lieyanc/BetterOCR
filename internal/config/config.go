// Package config manages BetterOCR's JSON configuration file. Missing files
// are initialized from an embedded template, and legacy single-provider files
// are migrated without losing model selections or credentials.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/lieyanc/BetterOCR/internal/model"
)

// DefaultPath 是默认的配置文件路径(相对当前工作目录)。
const DefaultPath = "betterocr.json"

// Config is the complete betterocr.json structure.
type Config struct {
	// Providers contains model catalogs and server-side credentials.
	Providers []model.Provider `json:"providers"`
	// Engines contains provider/model references. Repeating a reference enables
	// multi-sampling with the same model.
	Engines []string `json:"engines"`
	// Arbiter is a provider/model reference. Empty disables remote arbitration.
	Arbiter string `json:"arbiter"`
	// TimeoutSeconds 是单次识别的端到端超时,非正数按 120 处理。
	TimeoutSeconds int `json:"timeout_seconds"`
	// ServeAddr 是 -serve 模式的监听地址。
	ServeAddr string `json:"serve_addr"`
}

// Default 返回内置硬编码模板,与 README 示例保持一致。
func Default() Config {
	return Config{
		Providers: []model.Provider{
			{
				ID:      "openai",
				BaseURL: "https://api.openai.com/v1",
				APIKey:  "",
				Models: []model.Definition{
					{ID: "gpt-4.1-mini", Context: 1048576, Alias: "GPT-4.1 mini", API: model.APIOpenAIResponses},
					{ID: "gpt-4o-mini", Context: 128000, Alias: "GPT-4o mini", API: model.APIOpenAIChat},
				},
			},
			{
				ID:      "anthropic",
				BaseURL: "https://api.anthropic.com/v1",
				APIKey:  "",
				Models: []model.Definition{
					{ID: "claude-sonnet-4-20250514", Context: 200000, Alias: "Claude Sonnet 4", API: model.APIAnthropic},
				},
			},
		},
		Engines:        []string{"openai/gpt-4.1-mini", "openai/gpt-4o-mini"},
		Arbiter:        "anthropic/claude-sonnet-4-20250514",
		TimeoutSeconds: 120,
		ServeAddr:      "127.0.0.1:8787",
	}
}

// Timeout 把 TimeoutSeconds 换算成 time.Duration,非正数取默认 2 分钟。
func (c Config) Timeout() time.Duration {
	if c.TimeoutSeconds > 0 {
		return time.Duration(c.TimeoutSeconds) * time.Second
	}
	return 2 * time.Minute
}

// Action 描述 Load 对配置文件做了什么,供启动日志提示用户。
type Action int

const (
	// ActionNone 表示文件已存在且字段齐全,原样使用。
	ActionNone Action = iota
	// ActionReleased 表示文件不存在(或为空),已释放内置模板。
	ActionReleased
	// ActionSupplemented 表示文件缺少字段,已按模板补全并写回。
	ActionSupplemented
	// ActionMigrated 表示旧版单 Provider 配置已迁移到 providers 结构。
	ActionMigrated
)

// Load 读取 path 处的 JSON 配置:
//   - 文件不存在或为空 → 释放内置模板并返回模板值;
//   - 旧版单 Provider 结构 → 原地迁移到 providers 模型目录;
//   - 缺少字段 → 以模板补全缺失项并写回(文件中显式的空值视为用户决定,保留);
//   - 解析或引用校验失败 → 返回错误,绝不改动原文件。
func Load(path string) (Config, Action, error) {
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist), err == nil && len(bytes.TrimSpace(raw)) == 0:
		cfg := Default()
		if werr := write(path, cfg); werr != nil {
			return Config{}, ActionNone, fmt.Errorf("释放默认配置到 %s 失败: %w", path, werr)
		}
		return cfg, ActionReleased, nil
	case err != nil:
		return Config{}, ActionNone, err
	}

	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		return Config{}, ActionNone, fmt.Errorf("解析 %s 失败(不会改动该文件): %w", path, err)
	}
	if _, hasProviders := present["providers"]; !hasProviders && hasLegacyFields(present) {
		cfg, err := migrateLegacy(raw)
		if err != nil {
			return Config{}, ActionNone, fmt.Errorf("迁移 %s 失败(不会改动该文件): %w", path, err)
		}
		if err := cfg.Validate(); err != nil {
			return Config{}, ActionNone, fmt.Errorf("配置 %s 无效(不会改动该文件): %w", path, err)
		}
		if err := write(path, cfg); err != nil {
			return Config{}, ActionNone, fmt.Errorf("写回迁移配置 %s 失败: %w", path, err)
		}
		return cfg, ActionMigrated, nil
	}
	// 以模板为底座覆盖解析:文件里出现的字段(含显式空值)以文件为准,
	// 缺失的字段自然保留模板值。
	cfg := Default()
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, ActionNone, fmt.Errorf("解析 %s 失败(不会改动该文件): %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, ActionNone, fmt.Errorf("配置 %s 无效(不会改动该文件): %w", path, err)
	}

	for _, key := range fieldKeys() {
		if _, ok := present[key]; !ok {
			// 写回规范字段集;文件中的未知字段会在此时被清理
			if err := write(path, cfg); err != nil {
				return Config{}, ActionNone, fmt.Errorf("补全配置 %s 失败: %w", path, err)
			}
			return cfg, ActionSupplemented, nil
		}
	}
	return cfg, ActionNone, nil
}

// Validate checks provider/model identity and all configured selections.
func (c Config) Validate() error {
	providers := make(map[string]struct{}, len(c.Providers))
	refs := make(map[string]struct{})
	for pi, provider := range c.Providers {
		providerID := strings.TrimSpace(provider.ID)
		if providerID == "" {
			return fmt.Errorf("providers[%d].id 不能为空", pi)
		}
		if strings.Contains(providerID, "/") {
			return fmt.Errorf("provider %q 的 id 不能包含 /", providerID)
		}
		if _, exists := providers[providerID]; exists {
			return fmt.Errorf("provider id %q 重复", providerID)
		}
		providers[providerID] = struct{}{}
		if strings.TrimSpace(provider.BaseURL) == "" {
			return fmt.Errorf("provider %q 的 base_url 不能为空", providerID)
		}
		models := make(map[string]struct{}, len(provider.Models))
		for mi, definition := range provider.Models {
			modelID := strings.TrimSpace(definition.ID)
			if modelID == "" {
				return fmt.Errorf("provider %q 的 models[%d].id 不能为空", providerID, mi)
			}
			if _, exists := models[modelID]; exists {
				return fmt.Errorf("provider %q 的 model id %q 重复", providerID, modelID)
			}
			models[modelID] = struct{}{}
			if strings.TrimSpace(definition.Alias) == "" {
				return fmt.Errorf("模型 %q 的 alias 不能为空", model.Reference(providerID, modelID))
			}
			if definition.Context <= 0 {
				return fmt.Errorf("模型 %q 的 context 必须大于 0", model.Reference(providerID, modelID))
			}
			if !definition.API.Valid() {
				return fmt.Errorf("模型 %q 的 api %q 不受支持", model.Reference(providerID, modelID), definition.API)
			}
			refs[model.Reference(providerID, modelID)] = struct{}{}
		}
	}
	for _, ref := range c.Engines {
		if _, ok := refs[strings.TrimSpace(ref)]; !ok {
			return fmt.Errorf("engines 引用了未配置模型 %q", ref)
		}
	}
	if ref := strings.TrimSpace(c.Arbiter); ref != "" {
		if _, ok := refs[ref]; !ok {
			return fmt.Errorf("arbiter 引用了未配置模型 %q", ref)
		}
	}
	return nil
}

// Resolve looks up a provider/model reference and attaches its credentials.
func (c Config) Resolve(ref string) (model.Resolved, error) {
	ref = strings.TrimSpace(ref)
	for _, provider := range c.Providers {
		for _, definition := range provider.Models {
			if model.Reference(provider.ID, definition.ID) != ref {
				continue
			}
			return model.Resolved{
				Ref:        ref,
				ProviderID: strings.TrimSpace(provider.ID),
				BaseURL:    strings.TrimSpace(provider.BaseURL),
				APIKey:     provider.APIKey,
				ID:         strings.TrimSpace(definition.ID),
				Context:    definition.Context,
				Alias:      strings.TrimSpace(definition.Alias),
				API:        definition.API,
			}, nil
		}
	}
	return model.Resolved{}, fmt.Errorf("未配置模型 %q", ref)
}

// ResolveMany resolves model references while preserving order and repeats.
func (c Config) ResolveMany(refs []string) ([]model.Resolved, error) {
	out := make([]model.Resolved, 0, len(refs))
	for _, ref := range refs {
		resolved, err := c.Resolve(ref)
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}

type legacyConfig struct {
	Engines        []string `json:"engines"`
	Arbiter        string   `json:"arbiter"`
	BaseURL        string   `json:"base_url"`
	APIKey         string   `json:"api_key"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	ServeAddr      string   `json:"serve_addr"`
}

func hasLegacyFields(present map[string]json.RawMessage) bool {
	for _, key := range []string{"base_url", "api_key"} {
		if _, ok := present[key]; ok {
			return true
		}
	}
	return false
}

func migrateLegacy(raw []byte) (Config, error) {
	defaults := Default()
	legacy := legacyConfig{
		BaseURL:        "https://api.example.com/v1",
		TimeoutSeconds: defaults.TimeoutSeconds,
		ServeAddr:      defaults.ServeAddr,
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(legacy.BaseURL) == "" {
		legacy.BaseURL = "https://api.openai.com/v1"
	}
	provider := model.Provider{ID: "default", BaseURL: legacy.BaseURL, APIKey: legacy.APIKey}
	seen := make(map[string]struct{})
	addModel := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		provider.Models = append(provider.Models, model.Definition{
			ID: id, Context: 128000, Alias: id, API: model.APIOpenAIChat,
		})
	}
	engines := make([]string, 0, len(legacy.Engines))
	for _, id := range legacy.Engines {
		addModel(id)
		if strings.TrimSpace(id) != "" {
			engines = append(engines, model.Reference(provider.ID, id))
		}
	}
	addModel(legacy.Arbiter)
	arbiter := ""
	if strings.TrimSpace(legacy.Arbiter) != "" {
		arbiter = model.Reference(provider.ID, legacy.Arbiter)
	}
	return Config{
		Providers:      []model.Provider{provider},
		Engines:        engines,
		Arbiter:        arbiter,
		TimeoutSeconds: legacy.TimeoutSeconds,
		ServeAddr:      legacy.ServeAddr,
	}, nil
}

// fieldKeys 从 Config 的 json tag 提取全部字段名,避免手抄清单与结构体漂移。
func fieldKeys() []string {
	t := reflect.TypeOf(Config{})
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			keys = append(keys, name)
		}
	}
	return keys
}

// write 落盘为缩进 JSON;配置内含 api_key,权限收紧到仅本用户可读写。
func write(path string, cfg Config) error {
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
