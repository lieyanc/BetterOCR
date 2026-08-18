// Package agents provides VLM engines and arbiters for the OpenAI Chat,
// OpenAI Responses, and Anthropic Messages APIs.
package agents

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/lieyanc/BetterOCR/internal/agent"
	"github.com/lieyanc/BetterOCR/internal/arbiter"
	"github.com/lieyanc/BetterOCR/internal/model"
)

// VisionVLM uses a configured multimodal model as a base OCR engine.
type VisionVLM struct {
	AgentName string
	Model     model.Resolved
	Client    *http.Client
}

// NewVisionVLM creates a base VLM engine.
func NewVisionVLM(name string, resolved model.Resolved, client *http.Client) *VisionVLM {
	return &VisionVLM{AgentName: name, Model: resolved, Client: client}
}

// Name implements agent.Agent.
func (v *VisionVLM) Name() string { return v.AgentName }

const ocrSystem = `You are an OCR engine. Transcribe ALL text visible in the image, line by line, in natural reading order.

Output the transcription as plain text: one output line per line of text in the image. Nothing else.

Rules:
- Preserve characters exactly as printed: case, punctuation, digits, full-width/half-width forms.
- Do not merge separate lines and do not split a single line.
- Do not add line numbers, bullets, markdown, code fences, or any commentary.
- If the image contains no text, output nothing at all.`

// Recognize implements agent.Agent.
//
// 纯文本输出没有"解析失败"这一说,代价是模型的寒暄(比如开头一句
// "Here is the transcription:")会混进行里。这里不做启发式拦截——拦不准。
// 它会成为只有一个引擎产出的孤行,在融合层进入仲裁,由仲裁器看图判定
// "不存在于图中"而丢弃。让架构处理噪声,比让正则去猜可靠。
func (v *VisionVLM) Recognize(ctx context.Context, image []byte) (agent.Result, error) {
	content, err := vision(ctx, v.Model, v.Client, ocrSystem, "Transcribe the image.", image)
	if err != nil {
		return agent.Result{}, err
	}
	return agent.Result{Lines: splitLines(content)}, nil
}

// VisionEscalator uses a configured multimodal model to resolve disputed rows.
type VisionEscalator struct {
	Model  model.Resolved
	Client *http.Client
}

// NewVisionEscalator creates a dispute arbiter.
func NewVisionEscalator(resolved model.Resolved, client *http.Client) *VisionEscalator {
	return &VisionEscalator{Model: resolved, Client: client}
}

// Name implements arbiter.Escalator.
func (e *VisionEscalator) Name() string {
	return "arbiter:" + e.Model.DisplayName() + " (" + e.Model.ProviderName() + ")"
}

const escalatorSystem = `You are the arbiter in a multi-engine OCR system. Several fast OCR engines transcribed the same image; most lines agree, but the rows listed below are disputed. Look at the image and decide the correct text for each disputed row.

Output one plain-text line per disputed row, in exactly this form:

#<row> <the correct text>

Rules:
- Emit exactly one line per disputed row, reusing the row numbers given below.
- Read the actual image; do not simply pick the most common candidate.
- If a disputed row does not actually exist in the image, emit "#<row>" with nothing after it.
- Do not add commentary, markdown, or code fences.`

// Resolve implements arbiter.Escalator and batches all disputed rows in one call.
func (e *VisionEscalator) Resolve(ctx context.Context, image []byte, disputes []arbiter.Dispute) ([]arbiter.Resolution, error) {
	content, err := vision(ctx, e.Model, e.Client, escalatorSystem, disputesPrompt(disputes), image)
	if err != nil {
		return nil, err
	}
	var out []arbiter.Resolution
	for _, line := range strings.Split(stripFences(content), "\n") {
		if row, text, ok := parseRowLine(line); ok {
			out = append(out, arbiter.Resolution{Row: row, Text: text})
		}
	}
	// 一行都认不出来是真失败,必须报错:静默返回空会让所有分歧行悄悄
	// 走本地兜底,而 Stats.EscalationErr 是空的——问题被藏起来了。
	if len(out) == 0 {
		return nil, fmt.Errorf("仲裁输出中没有 \"#<row> <text>\" 形式的行: %.200s", content)
	}
	return out, nil
}

// parseRowLine 解析一行 "#<row> <text>"。行号后必须紧跟空白或直接结束,
// 以免把正文里的 "#1234.5" 误读成行号;认不出的行(寒暄、空行)直接忽略。
// "#<row>" 后面为空表示仲裁器判定该行不存在于图中。
func parseRowLine(s string) (int, string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "#") {
		return 0, "", false
	}
	rest := s[1:]
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0, "", false
	}
	row, err := strconv.Atoi(rest[:digits])
	if err != nil {
		return 0, "", false
	}
	text := rest[digits:]
	if text != "" && text[0] != ' ' && text[0] != '\t' {
		return 0, "", false
	}
	return row, strings.TrimSpace(text), true
}

func disputesPrompt(disputes []arbiter.Dispute) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Disputed rows (%d):\n", len(disputes))
	for _, d := range disputes {
		fmt.Fprintf(&sb, "\n#%d", d.Row)
		if d.Before != "" || d.After != "" {
			fmt.Fprintf(&sb, " (between %q and %q)", d.Before, d.After)
		}
		sb.WriteString(":\n")
		// 候选只列文本,不列任何"票数"或"置信度"——上面刚要求它看图判断,
		// 再递上一个人气指标就是在给它反向锚点。
		for _, c := range d.Candidates {
			fmt.Fprintf(&sb, "  - engine %q read: %q\n", c.Agent, c.Text)
		}
	}
	sb.WriteString("\nReturn one \"#<row> <text>\" line per disputed row.")
	return sb.String()
}

func vision(ctx context.Context, resolved model.Resolved, client *http.Client, system, userText string, image []byte) (string, error) {
	mediaType := http.DetectContentType(image)
	if !strings.HasPrefix(mediaType, "image/") {
		return "", fmt.Errorf("输入不是可识别的图片格式: %s", mediaType)
	}
	if client == nil {
		client = http.DefaultClient
	}
	switch resolved.API {
	case model.APIOpenAIChatCompletions:
		return openAIChat(ctx, resolved, client, system, userText, mediaType, image)
	case model.APIOpenAIResponses:
		return openAIResponses(ctx, resolved, client, system, userText, mediaType, image)
	case model.APIAnthropicMessages:
		return anthropicMessages(ctx, resolved, client, system, userText, mediaType, image)
	default:
		return "", fmt.Errorf("模型 %s 使用了不受支持的 API %q", resolved.Ref, resolved.API)
	}
}

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

func openAIChat(ctx context.Context, resolved model.Resolved, client *http.Client, system, userText, mediaType string, image []byte) (string, error) {
	body := chatRequest{
		Model: resolved.ID,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: []chatContentPart{
				{Type: "image_url", ImageURL: &chatImageURL{URL: imageDataURL(mediaType, image)}},
				{Type: "text", Text: userText},
			}},
		},
		MaxTokens: maxOutputTokens(resolved.Context),
	}
	raw, err := postJSON(ctx, resolved, client, "/chat/completions", body, false)
	if err != nil {
		return "", err
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("模型 %s 返回了无效 JSON: %w", resolved.Ref, err)
	}
	if len(response.Choices) == 0 {
		return "", fmt.Errorf("模型 %s 未返回任何 choice", resolved.Ref)
	}
	text := textFromContent(response.Choices[0].Message.Content)
	if text == "" {
		return "", fmt.Errorf("模型 %s 未返回文本内容", resolved.Ref)
	}
	return text, nil
}

type responsesRequest struct {
	Model           string             `json:"model"`
	Instructions    string             `json:"instructions"`
	Input           []responsesMessage `json:"input"`
	MaxOutputTokens int                `json:"max_output_tokens,omitempty"`
}

type responsesMessage struct {
	Role    string                 `json:"role"`
	Content []responsesContentPart `json:"content"`
}

type responsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

func openAIResponses(ctx context.Context, resolved model.Resolved, client *http.Client, system, userText, mediaType string, image []byte) (string, error) {
	body := responsesRequest{
		Model:        resolved.ID,
		Instructions: system,
		Input: []responsesMessage{{
			Role: "user",
			Content: []responsesContentPart{
				{Type: "input_image", ImageURL: imageDataURL(mediaType, image)},
				{Type: "input_text", Text: userText},
			},
		}},
		MaxOutputTokens: maxOutputTokens(resolved.Context),
	}
	raw, err := postJSON(ctx, resolved, client, "/responses", body, false)
	if err != nil {
		return "", err
	}
	var response struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("模型 %s 返回了无效 JSON: %w", resolved.Ref, err)
	}
	if strings.TrimSpace(response.OutputText) != "" {
		return response.OutputText, nil
	}
	var parts []string
	for _, item := range response.Output {
		for _, content := range item.Content {
			if content.Type == "output_text" && content.Text != "" {
				parts = append(parts, content.Text)
			}
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("模型 %s 未返回 output_text", resolved.Ref)
	}
	return strings.Join(parts, "\n"), nil
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
}

type anthropicMessage struct {
	Role    string                 `json:"role"`
	Content []anthropicContentPart `json:"content"`
}

type anthropicContentPart struct {
	Type   string           `json:"type"`
	Text   string           `json:"text,omitempty"`
	Source *anthropicSource `json:"source,omitempty"`
}

type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

func anthropicMessages(ctx context.Context, resolved model.Resolved, client *http.Client, system, userText, mediaType string, image []byte) (string, error) {
	body := anthropicRequest{
		Model:  resolved.ID,
		System: system,
		Messages: []anthropicMessage{{
			Role: "user",
			Content: []anthropicContentPart{
				{Type: "image", Source: &anthropicSource{
					Type: "base64", MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(image),
				}},
				{Type: "text", Text: userText},
			},
		}},
		MaxTokens: maxOutputTokens(resolved.Context),
	}
	raw, err := postJSON(ctx, resolved, client, "/messages", body, true)
	if err != nil {
		return "", err
	}
	var response struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("模型 %s 返回了无效 JSON: %w", resolved.Ref, err)
	}
	var parts []string
	for _, content := range response.Content {
		if content.Type == "text" && content.Text != "" {
			parts = append(parts, content.Text)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("模型 %s 未返回文本内容", resolved.Ref)
	}
	return strings.Join(parts, "\n"), nil
}

func postJSON(ctx context.Context, resolved model.Resolved, client *http.Client, endpoint string, body any, anthropic bool) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(resolved.BaseURL, "/")+endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if anthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
		if resolved.APIKey != "" {
			req.Header.Set("x-api-key", resolved.APIKey)
		}
	} else if resolved.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+resolved.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := strings.TrimSpace(string(responseBody))
		parsed := parseAPIError(responseBody)
		if parsed != "" {
			message = parsed
		}
		return nil, fmt.Errorf("模型 %s 请求失败 (HTTP %d): %.300s", resolved.Ref, resp.StatusCode, message)
	}
	if message := parseAPIError(responseBody); message != "" {
		return nil, fmt.Errorf("模型 %s 返回错误: %s", resolved.Ref, message)
	}
	return responseBody, nil
}

func parseAPIError(body []byte) string {
	var parsed struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error != nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return ""
}

func imageDataURL(mediaType string, image []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(image)
}

func maxOutputTokens(contextWindow int) int {
	const desired = 8192
	if half := contextWindow / 2; half > 0 && half < desired {
		return half
	}
	return desired
}

func textFromContent(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if (part.Type == "text" || part.Type == "output_text") && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// splitLines 把模型的纯文本输出切成行:去掉整体包裹的 markdown 围栏,
// 丢弃空行,每行去首尾空白。留白不是识别分歧,不值得进入对齐。
func splitLines(s string) []string {
	raw := strings.Split(stripFences(s), "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

// stripFences 去掉整体包裹输出的 markdown 代码围栏(```、```text 等)。
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	i := strings.IndexByte(s, '\n')
	if i < 0 {
		return ""
	}
	s = s[i+1:]
	if j := strings.LastIndex(s, "```"); j >= 0 {
		s = s[:j]
	}
	return strings.TrimSpace(s)
}
