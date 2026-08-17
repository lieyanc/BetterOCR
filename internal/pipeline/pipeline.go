// Package pipeline 把引擎组装、并发识别与融合串成一次完整调用。
// CLI 与 Web 服务复用同一条流水线:浏览器里的一次识别与命令行
// 的一次调用在行为上完全等价。
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/lieyanc/BetterOCR/internal/agent"
	"github.com/lieyanc/BetterOCR/internal/agents"
	"github.com/lieyanc/BetterOCR/internal/arbiter"
)

// DefaultBaseURL 是 base_url 未配置(或被显式清空)时的兜底端点。
const DefaultBaseURL = "https://api.openai.com/v1"

// Config 描述一次识别的全部参数。
type Config struct {
	// Engines 是基础 VLM 模型列表;同一模型重复出现即多路采样。
	Engines []string
	// Arbiter 是仲裁模型;为空则分歧行退化为本地择优。
	Arbiter string
	// BaseURL 是 OpenAI 兼容端点地址,为空时回退 DefaultBaseURL;
	// APIKey 为空时不发送认证头。
	BaseURL string
	APIKey  string
	// HTTPClient 为 nil 时使用 http.DefaultClient。
	HTTPClient *http.Client
}

// Run 执行一次完整识别:并发引擎 → 行级对齐 → 共识/仲裁融合。
func Run(ctx context.Context, cfg Config, image []byte) (arbiter.Final, error) {
	if len(cfg.Engines) == 0 {
		return arbiter.Final{}, errors.New("未配置任何引擎模型")
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	oc := agents.OpenAIConfig{BaseURL: baseURL, APIKey: cfg.APIKey, HTTPClient: cfg.HTTPClient}

	reg := agent.NewRegistry()
	for i, model := range cfg.Engines {
		// 名称带序号,同一模型多路采样时互不冲突
		reg.MustRegister(agents.NewOpenAIVLM(fmt.Sprintf("%s#%d", model, i+1), model, oc))
	}

	arb := arbiter.New()
	if cfg.Arbiter != "" {
		arb.Escalator = agents.NewOpenAIEscalator(cfg.Arbiter, oc)
	}

	results := agent.NewCoordinator(reg).RunConcurrent(ctx, image)
	return arb.Fuse(ctx, image, results), nil
}

// SplitList 把逗号分隔的模型列表拆成去空白的非空项。
func SplitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
