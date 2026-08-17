// Package agents 提供内置的 Agent 实现。
package agents

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"

	"github.com/lieyanc/BetterOCR/internal/agent"
)

// 生成一张白底黑字图片的简易位图字体，仅用于 demo/mock。

// MockAgent 返回固定文本的假 Agent，用于演示与测试。
// Text 与 Confidence 决定其行为；Fail 为 true 时总是返回错误。
type MockAgent struct {
	AgentName  string
	Text       string
	Confidence float64
	Fail       bool
}

// Name 实现 agent.Agent。
func (m *MockAgent) Name() string { return m.AgentName }

// Recognize 实现 agent.Agent。
func (m *MockAgent) Recognize(ctx context.Context, image []byte) (agent.Result, error) {
	if m.Fail {
		return agent.Result{}, errors.New("mock agent failure")
	}
	return agent.Result{Text: m.Text, Confidence: m.Confidence}, nil
}

// StubImage 生成一张 1x1 的 PNG 图片字节，供 mock 使用。
func StubImage() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.White)
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
