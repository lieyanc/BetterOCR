// Package config manages BetterOCR's JSON configuration file. Missing files
// are initialized from an embedded template, and incomplete files are
// supplemented without losing model selections or credentials.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/lieyanc/BetterOCR/internal/model"
)

// DefaultPath 是默认的配置文件路径(相对当前工作目录)。
const DefaultPath = "data/config.json"

const (
	defaultTimeoutSeconds = 120
	defaultMaxAttempts    = 2
)

// Config is the complete data/config.json structure.
type Config struct {
	// Providers contains model catalogs and server-side credentials.
	Providers []model.Provider `json:"providers"`
	// Engines contains provider/model references. Repeating a reference enables
	// multi-sampling with the same model.
	Engines []string `json:"engines"`
	// Arbiter is a provider/model reference. Empty disables remote arbitration.
	Arbiter string `json:"arbiter"`
	// EngineTimeoutSeconds 是每个基础模型每次尝试的超时秒数。
	EngineTimeoutSeconds int `json:"engine_timeout_seconds"`
	// ArbiterTimeoutSeconds 是仲裁模型每次尝试的超时秒数。
	ArbiterTimeoutSeconds int `json:"arbiter_timeout_seconds"`
	// EngineMaxAttempts 是单个基础模型的最大尝试次数,1 表示不重试。
	EngineMaxAttempts int `json:"engine_max_attempts"`
	// ArbiterMaxAttempts 是仲裁模型的最大尝试次数,1 表示不重试。
	ArbiterMaxAttempts int `json:"arbiter_max_attempts"`
	// TimeoutSeconds 仅用于读取旧配置,规范写回时会移除。
	TimeoutSeconds int `json:"timeout_seconds,omitempty" config:"legacy"`
	// ServeAddr 是 -serve 模式的监听地址。
	ServeAddr string `json:"serve_addr"`
}

// Default 返回内置硬编码模板,与 README 示例保持一致。
func Default() Config {
	return Config{
		Providers: []model.Provider{
			{
				ID:      "openai",
				Alias:   "OpenAI",
				BaseURL: "https://api.openai.com/v1",
				APIKey:  "",
				Models: []model.Definition{
					{ID: "gpt-4.1-mini", Context: 1048576, Alias: "GPT-4.1 mini", API: model.APIOpenAIResponses},
					{ID: "gpt-4o-mini", Context: 128000, Alias: "GPT-4o mini", API: model.APIOpenAIChatCompletions},
				},
			},
			{
				ID:      "anthropic",
				Alias:   "Anthropic",
				BaseURL: "https://api.anthropic.com/v1",
				APIKey:  "",
				Models: []model.Definition{
					{ID: "claude-sonnet-4-20250514", Context: 200000, Alias: "Claude Sonnet 4", API: model.APIAnthropicMessages},
				},
			},
		},
		Engines:               []string{"openai/gpt-4.1-mini", "openai/gpt-4o-mini"},
		Arbiter:               "anthropic/claude-sonnet-4-20250514",
		EngineTimeoutSeconds:  defaultTimeoutSeconds,
		ArbiterTimeoutSeconds: defaultTimeoutSeconds,
		EngineMaxAttempts:     defaultMaxAttempts,
		ArbiterMaxAttempts:    defaultMaxAttempts,
		ServeAddr:             "127.0.0.1:8787",
	}
}

// EngineTimeout returns the independent timeout budget for each engine attempt.
func (c Config) EngineTimeout() time.Duration {
	return time.Duration(c.normalized().EngineTimeoutSeconds) * time.Second
}

// ArbiterTimeout returns the independent timeout budget for each arbiter attempt.
func (c Config) ArbiterTimeout() time.Duration {
	return time.Duration(c.normalized().ArbiterTimeoutSeconds) * time.Second
}

// EngineAttempts returns the maximum attempts for each individual engine.
func (c Config) EngineAttempts() int { return c.normalized().EngineMaxAttempts }

// ArbiterAttempts returns the maximum attempts for arbitration only.
func (c Config) ArbiterAttempts() int { return c.normalized().ArbiterMaxAttempts }

func (c Config) normalized() Config {
	if c.EngineTimeoutSeconds == 0 {
		if c.TimeoutSeconds > 0 {
			c.EngineTimeoutSeconds = c.TimeoutSeconds
		} else {
			c.EngineTimeoutSeconds = defaultTimeoutSeconds
		}
	}
	if c.ArbiterTimeoutSeconds == 0 {
		if c.TimeoutSeconds > 0 {
			c.ArbiterTimeoutSeconds = c.TimeoutSeconds
		} else {
			c.ArbiterTimeoutSeconds = defaultTimeoutSeconds
		}
	}
	if c.EngineMaxAttempts == 0 {
		c.EngineMaxAttempts = defaultMaxAttempts
	}
	if c.ArbiterMaxAttempts == 0 {
		c.ArbiterMaxAttempts = defaultMaxAttempts
	}
	c.TimeoutSeconds = 0
	return c
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
)

// Load 读取 path 处的 JSON 配置:
//   - 文件不存在或为空 → 释放内置模板并返回模板值;
//   - 缺少字段 → 以模板补全缺失项并写回(文件中显式的空值视为用户决定,
//     保留;唯一例外是 provider 的空 alias,会补为模板名或 id);
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
	supplemented := false
	// 以模板为底座覆盖解析:文件里出现的字段(含显式空值)以文件为准,
	// 缺失的字段自然保留模板值。数组字段(providers)按整体覆盖,否则
	// Go 的按索引合并会让文件条目继承模板同位置条目的残留字段。
	cfg := Default()
	cfg.Providers = nil
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, ActionNone, fmt.Errorf("解析 %s 失败(不会改动该文件): %w", path, err)
	}
	if _, legacy := present["timeout_seconds"]; legacy {
		if _, current := present["engine_timeout_seconds"]; !current && cfg.TimeoutSeconds > 0 {
			cfg.EngineTimeoutSeconds = cfg.TimeoutSeconds
		}
		if _, current := present["arbiter_timeout_seconds"]; !current && cfg.TimeoutSeconds > 0 {
			cfg.ArbiterTimeoutSeconds = cfg.TimeoutSeconds
		}
		cfg.TimeoutSeconds = 0
		supplemented = true
	}
	if _, ok := present["providers"]; !ok {
		cfg.Providers = Default().Providers
	} else if supplementProviderAliases(cfg.Providers) {
		supplemented = true
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, ActionNone, fmt.Errorf("配置 %s 无效(不会改动该文件): %w", path, err)
	}

	for _, key := range fieldKeys() {
		if _, ok := present[key]; !ok || supplemented {
			// 写回规范字段集;文件中的未知字段会在此时被清理
			if err := write(path, cfg); err != nil {
				return Config{}, ActionNone, fmt.Errorf("补全配置 %s 失败: %w", path, err)
			}
			return cfg, ActionSupplemented, nil
		}
	}
	return cfg, ActionNone, nil
}

// Save validates cfg and atomically persists it as the single configuration
// source shared by CLI and Web mode.
func Save(path string, cfg Config) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("配置文件路径不能为空")
	}
	cfg = cfg.normalized()
	if err := cfg.Validate(); err != nil {
		return err
	}
	return write(path, cfg)
}

// supplementProviderAliases 给 alias 为空(缺失或显式空)的 provider 补显示名:
// 模板中有同 id 的 provider 就沿用模板 alias,否则回退 id 本身。
// 返回是否有补全动作,供 Load 决定写回。
func supplementProviderAliases(providers []model.Provider) bool {
	changed := false
	for i := range providers {
		if strings.TrimSpace(providers[i].Alias) != "" {
			continue
		}
		alias := strings.TrimSpace(providers[i].ID)
		for _, tp := range Default().Providers {
			if tp.ID == providers[i].ID && strings.TrimSpace(tp.Alias) != "" {
				alias = strings.TrimSpace(tp.Alias)
				break
			}
		}
		providers[i].Alias = alias
		changed = true
	}
	return changed
}

// Validate checks provider/model identity and all configured selections.
func (c Config) Validate() error {
	c = c.normalized()
	if c.EngineTimeoutSeconds <= 0 {
		return errors.New("engine_timeout_seconds 必须大于 0")
	}
	if c.ArbiterTimeoutSeconds <= 0 {
		return errors.New("arbiter_timeout_seconds 必须大于 0")
	}
	if c.EngineMaxAttempts < 1 || c.EngineMaxAttempts > 10 {
		return errors.New("engine_max_attempts 必须在 1 到 10 之间")
	}
	if c.ArbiterMaxAttempts < 1 || c.ArbiterMaxAttempts > 10 {
		return errors.New("arbiter_max_attempts 必须在 1 到 10 之间")
	}
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
				Ref:           ref,
				ProviderID:    strings.TrimSpace(provider.ID),
				ProviderAlias: strings.TrimSpace(provider.Alias),
				BaseURL:       strings.TrimSpace(provider.BaseURL),
				APIKey:        provider.APIKey,
				ID:            strings.TrimSpace(definition.ID),
				Context:       definition.Context,
				Alias:         strings.TrimSpace(definition.Alias),
				API:           definition.API,
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

// fieldKeys 从 Config 的 json tag 提取全部字段名,避免手抄清单与结构体漂移。
func fieldKeys() []string {
	t := reflect.TypeOf(Config{})
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Tag.Get("config") == "legacy" {
			continue
		}
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			keys = append(keys, name)
		}
	}
	return keys
}

// write 原子落盘为缩进 JSON;配置内含 api_key,权限收紧到仅本用户可读写。
func write(path string, cfg Config) error {
	cfg = cfg.normalized()
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".betterocr-config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
