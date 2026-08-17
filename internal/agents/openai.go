// Package agents 提供基于 OpenAI 兼容 API 的 VLM 引擎与仲裁器实现。
// 任何暴露 /chat/completions 的服务(OpenAI、SiliconFlow、DashScope、
// vLLM、Ollama 等)都可直接接入。
package agents

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lieyanc/BetterOCR/internal/agent"
	"github.com/lieyanc/BetterOCR/internal/arbiter"
)

// OpenAIConfig 是 OpenAI 兼容端点的连接配置。
type OpenAIConfig struct {
	// BaseURL 形如 https://api.openai.com/v1,末尾不带斜杠。
	BaseURL string
	// APIKey 为空时不发送 Authorization 头(本地 vLLM/Ollama 常见)。
	APIKey string
	// HTTPClient 为 nil 时使用 http.DefaultClient;超时由调用方 ctx 控制。
	HTTPClient *http.Client
}

func (c OpenAIConfig) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// OpenAIVLM 用 OpenAI 兼容的视觉模型做基础 OCR 引擎。
type OpenAIVLM struct {
	AgentName string
	Model     string
	Config    OpenAIConfig
}

// NewOpenAIVLM 创建一个基础 VLM 引擎。
func NewOpenAIVLM(name, model string, cfg OpenAIConfig) *OpenAIVLM {
	return &OpenAIVLM{AgentName: name, Model: model, Config: cfg}
}

// Name 实现 agent.Agent。
func (o *OpenAIVLM) Name() string { return o.AgentName }

const ocrSystem = `You are an OCR engine. Transcribe ALL text visible in the image, line by line, in natural reading order.

Output ONLY a JSON array, no markdown fences, no commentary. One element per text line:
[{"text": "...", "confidence": 0.95}, ...]

Rules:
- Preserve characters exactly as printed: case, punctuation, digits, full-width/half-width forms.
- Do not merge separate lines and do not split a single line.
- "confidence" is your certainty in [0,1] that the line is transcribed exactly.
- If the image contains no text, output [].`

// Recognize 实现 agent.Agent。
func (o *OpenAIVLM) Recognize(ctx context.Context, image []byte) (agent.Result, error) {
	content, err := chatVision(ctx, o.Config, o.Model, ocrSystem, "Transcribe the image.", image)
	if err != nil {
		return agent.Result{}, err
	}
	raw, err := extractJSON(content)
	if err != nil {
		return agent.Result{}, err
	}
	var parsed []struct {
		Text       string  `json:"text"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return agent.Result{}, fmt.Errorf("解析模型输出失败: %w", err)
	}
	lines := make([]agent.Line, 0, len(parsed))
	for _, p := range parsed {
		if strings.TrimSpace(p.Text) == "" {
			continue
		}
		lines = append(lines, agent.Line{Text: p.Text, Confidence: p.Confidence})
	}
	return agent.Result{Lines: lines}, nil
}

// OpenAIEscalator 用更强的 OpenAI 兼容视觉模型裁定分歧行。
type OpenAIEscalator struct {
	Model  string
	Config OpenAIConfig
}

// NewOpenAIEscalator 创建仲裁器。
func NewOpenAIEscalator(model string, cfg OpenAIConfig) *OpenAIEscalator {
	return &OpenAIEscalator{Model: model, Config: cfg}
}

// Name 实现 arbiter.Escalator。
func (e *OpenAIEscalator) Name() string { return "arbiter:" + e.Model }

const escalatorSystem = `You are the arbiter in a multi-engine OCR system. Several fast OCR engines transcribed the same image; most lines agree, but the rows listed below are disputed. Look at the image and decide the correct text for each disputed row.

Output ONLY a JSON array, no markdown fences, no commentary:
[{"row": 3, "text": "the correct text", "confidence": 0.98}, ...]

Rules:
- Return exactly one element per disputed row, keeping the given row numbers.
- Read the actual image; do not simply pick the most popular candidate.
- If a disputed line does not actually exist in the image, return an empty string for "text".
- "confidence" is your certainty in [0,1].`

// Resolve 实现 arbiter.Escalator:把一张图的全部分歧打包成一次调用。
func (e *OpenAIEscalator) Resolve(ctx context.Context, image []byte, disputes []arbiter.Dispute) ([]arbiter.Resolution, error) {
	content, err := chatVision(ctx, e.Config, e.Model, escalatorSystem, disputesPrompt(disputes), image)
	if err != nil {
		return nil, err
	}
	raw, err := extractJSON(content)
	if err != nil {
		return nil, err
	}
	var parsed []struct {
		Row        int     `json:"row"`
		Text       string  `json:"text"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("解析仲裁输出失败: %w", err)
	}
	out := make([]arbiter.Resolution, 0, len(parsed))
	for _, p := range parsed {
		out = append(out, arbiter.Resolution{Row: p.Row, Text: p.Text, Confidence: p.Confidence})
	}
	return out, nil
}

// disputesPrompt 把分歧渲染成给仲裁模型看的文本。
func disputesPrompt(disputes []arbiter.Dispute) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Disputed rows (%d):\n", len(disputes))
	for _, d := range disputes {
		fmt.Fprintf(&sb, "\nrow %d", d.Row)
		if d.Before != "" || d.After != "" {
			fmt.Fprintf(&sb, " (between %q and %q)", d.Before, d.After)
		}
		sb.WriteString(":\n")
		for _, c := range d.Candidates {
			fmt.Fprintf(&sb, "  - engine %q (confidence %.2f): %q\n", c.Agent, c.Confidence, c.Text)
		}
	}
	sb.WriteString("\nReturn the corrected text for each row as specified.")
	return sb.String()
}

// ---- OpenAI 兼容 /chat/completions 底层调用 ----

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string 或 []contentPart
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// chatVision 发送一次带图请求,返回模型文本输出。
func chatVision(ctx context.Context, cfg OpenAIConfig, model, system, userText string, image []byte) (string, error) {
	mediaType := http.DetectContentType(image)
	if !strings.HasPrefix(mediaType, "image/") {
		return "", fmt.Errorf("输入不是可识别的图片格式: %s", mediaType)
	}
	dataURL := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(image)

	body, err := json.Marshal(chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: []contentPart{
				{Type: "image_url", ImageURL: &imageURL{URL: dataURL}},
				{Type: "text", Text: userText},
			}},
		},
		MaxTokens: 8192,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := cfg.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", err
	}

	var parsed chatResponse
	// 先尝试解析结构化错误信息,拿不到再退回原始响应体。
	_ = json.Unmarshal(respBody, &parsed)
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(respBody))
		if parsed.Error != nil && parsed.Error.Message != "" {
			msg = parsed.Error.Message
		}
		return "", fmt.Errorf("模型 %s 请求失败 (HTTP %d): %.300s", model, resp.StatusCode, msg)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("模型 %s 返回错误: %s", model, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("模型 %s 未返回任何 choice", model)
	}
	return parsed.Choices[0].Message.Content, nil
}

// extractJSON 从模型输出中截取首个 '[' 到最后一个 ']' 的 JSON 数组,
// 容忍模型偶尔加上的代码围栏或说明文字。
func extractJSON(s string) ([]byte, error) {
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("模型输出中未找到 JSON 数组: %.200s", s)
	}
	return []byte(s[start : end+1]), nil
}
