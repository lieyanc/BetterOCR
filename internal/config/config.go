// Package config 管理 BetterOCR 的 JSON 配置文件:模板硬编码在二进制内,
// 启动时文件不存在则释放模板,存在但缺字段则按模板补全并写回;
// 无法解析的文件绝不改动。全部运行参数只来自该文件,不读取环境变量。
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
)

// DefaultPath 是默认的配置文件路径(相对当前工作目录)。
const DefaultPath = "betterocr.json"

// Config 是 betterocr.json 的完整结构,同时是内置模板的载体。
type Config struct {
	// Engines 是基础 VLM 模型列表;同一模型重复出现即多路采样。
	Engines []string `json:"engines"`
	// Arbiter 是仲裁模型;显式置空则分歧行退化为本地择优。
	Arbiter string `json:"arbiter"`
	// BaseURL 是 OpenAI 兼容端点;置空回退官方默认地址。
	BaseURL string `json:"base_url"`
	// APIKey 为空时不发送认证头(本地 vLLM/Ollama 常见)。
	APIKey string `json:"api_key"`
	// TimeoutSeconds 是单次识别的端到端超时,非正数按 120 处理。
	TimeoutSeconds int `json:"timeout_seconds"`
	// ServeAddr 是 -serve 模式的监听地址。
	ServeAddr string `json:"serve_addr"`
}

// Default 返回内置硬编码模板,与 README 示例保持一致。
func Default() Config {
	return Config{
		Engines:        []string{"qwen2.5-vl-7b", "qwen2.5-vl-7b", "glm-4v-9b"},
		Arbiter:        "qwen2.5-vl-72b",
		BaseURL:        "https://api.siliconflow.cn/v1",
		APIKey:         "",
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
)

// Load 读取 path 处的 JSON 配置:
//   - 文件不存在或为空 → 释放内置模板并返回模板值;
//   - 缺少字段 → 以模板补全缺失项并写回(文件中显式的空值视为用户决定,保留);
//   - 解析失败 → 返回错误,绝不改动原文件。
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
	// 以模板为底座覆盖解析:文件里出现的字段(含显式空值)以文件为准,
	// 缺失的字段自然保留模板值。
	cfg := Default()
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, ActionNone, fmt.Errorf("解析 %s 失败(不会改动该文件): %w", path, err)
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
	return os.WriteFile(path, append(out, '\n'), 0o600)
}
