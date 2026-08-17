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
	"github.com/lieyanc/BetterOCR/internal/model"
)

// Config 描述一次识别的全部参数。
type Config struct {
	// Engines are fully resolved provider models. Repeats enable multi-sampling.
	Engines []model.Resolved
	// Arbiter is nil when disputed rows should use the local fallback.
	Arbiter *model.Resolved
	// HTTPClient 为 nil 时使用 http.DefaultClient。
	HTTPClient *http.Client
}

// Run 执行一次完整识别:并发引擎 → 行级对齐 → 共识/仲裁融合。
func Run(ctx context.Context, cfg Config, image []byte) (arbiter.Final, error) {
	if len(cfg.Engines) == 0 {
		return arbiter.Final{}, errors.New("未配置任何引擎模型")
	}

	reg := agent.NewRegistry()
	for i, resolved := range cfg.Engines {
		// Provider and sequence keep names unique even when aliases are repeated.
		name := fmt.Sprintf("%s · %s#%d", resolved.DisplayName(), resolved.ProviderID, i+1)
		reg.MustRegister(agents.NewVisionVLM(name, resolved, cfg.HTTPClient))
	}

	arb := arbiter.New()
	if cfg.Arbiter != nil {
		arb.Escalator = agents.NewVisionEscalator(*cfg.Arbiter, cfg.HTTPClient)
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
