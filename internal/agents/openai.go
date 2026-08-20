// Package agents provides VLM engines and arbiters for the OpenAI Chat,
// OpenAI Responses, and Anthropic Messages APIs.
package agents

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	// OnDelta receives separately classified thinking and output fragments.
	OnDelta func(StreamDelta)
}

// StreamKind separates model reasoning from text that belongs in OCR output.
type StreamKind string

const (
	StreamThinking StreamKind = "thinking"
	StreamOutput   StreamKind = "output"
)

// StreamDelta is one classified fragment from an upstream model response.
type StreamDelta struct {
	Kind StreamKind
	Text string
}

type modelOutput struct {
	Text     string
	Thinking string
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
	content, err := vision(ctx, v.Model, v.Client, ocrSystem, "Transcribe the image.", image, v.OnDelta)
	if err != nil {
		return agent.Result{}, err
	}
	return agent.Result{Lines: splitLines(content)}, nil
}

// VisionEscalator uses a configured multimodal model to resolve disputed rows.
type VisionEscalator struct {
	Model  model.Resolved
	Client *http.Client
	// OnDelta receives separately classified arbitration fragments.
	OnDelta func(StreamDelta)
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
	content, err := vision(ctx, e.Model, e.Client, escalatorSystem, disputesPrompt(disputes), image, e.OnDelta)
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

func vision(ctx context.Context, resolved model.Resolved, client *http.Client, system, userText string, image []byte, onDelta func(StreamDelta)) (string, error) {
	mediaType := http.DetectContentType(image)
	if !strings.HasPrefix(mediaType, "image/") {
		return "", fmt.Errorf("输入不是可识别的图片格式: %s", mediaType)
	}
	if client == nil {
		client = http.DefaultClient
	}
	switch resolved.API {
	case model.APIOpenAIChatCompletions:
		return openAIChat(ctx, resolved, client, system, userText, mediaType, image, onDelta)
	case model.APIOpenAIResponses:
		return openAIResponses(ctx, resolved, client, system, userText, mediaType, image, onDelta)
	case model.APIAnthropicMessages:
		return anthropicMessages(ctx, resolved, client, system, userText, mediaType, image, onDelta)
	default:
		return "", fmt.Errorf("模型 %s 使用了不受支持的 API %q", resolved.Ref, resolved.API)
	}
}

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	Stream    bool          `json:"stream"`
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

func openAIChat(ctx context.Context, resolved model.Resolved, client *http.Client, system, userText, mediaType string, image []byte, onDelta func(StreamDelta)) (string, error) {
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
		Stream:    true,
	}
	raw, streamed, err := postJSONStream(ctx, resolved, client, "/chat/completions", body, false, onDelta, chatDelta)
	if err != nil {
		return "", err
	}
	if streamed != "" {
		return streamed, nil
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
				Thinking         json.RawMessage `json:"thinking"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("模型 %s 返回了无效 JSON: %w", resolved.Ref, err)
	}
	if len(response.Choices) == 0 {
		return "", fmt.Errorf("模型 %s 未返回任何 choice", resolved.Ref)
	}
	message := response.Choices[0].Message
	text := textFromContent(message.Content)
	if text == "" {
		return "", fmt.Errorf("模型 %s 未返回文本内容", resolved.Ref)
	}
	emitModelOutput(onDelta, modelOutput{
		Text:     text,
		Thinking: firstTextContent(message.ReasoningContent, message.Reasoning, message.Thinking),
	})
	return text, nil
}

type responsesRequest struct {
	Model           string             `json:"model"`
	Instructions    string             `json:"instructions"`
	Input           []responsesMessage `json:"input"`
	MaxOutputTokens int                `json:"max_output_tokens,omitempty"`
	Stream          bool               `json:"stream"`
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

func openAIResponses(ctx context.Context, resolved model.Resolved, client *http.Client, system, userText, mediaType string, image []byte, onDelta func(StreamDelta)) (string, error) {
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
		Stream:          true,
	}
	raw, streamed, err := postJSONStream(ctx, resolved, client, "/responses", body, false, onDelta, responsesDelta)
	if err != nil {
		return "", err
	}
	if streamed != "" {
		return streamed, nil
	}
	var response struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("模型 %s 返回了无效 JSON: %w", resolved.Ref, err)
	}
	var textParts, thinkingParts []string
	for _, item := range response.Output {
		for _, content := range item.Content {
			if content.Type == "output_text" && content.Text != "" {
				textParts = append(textParts, content.Text)
			}
		}
		for _, summary := range item.Summary {
			if (summary.Type == "summary_text" || summary.Type == "text") && summary.Text != "" {
				thinkingParts = append(thinkingParts, summary.Text)
			}
		}
	}
	text := response.OutputText
	if strings.TrimSpace(text) == "" {
		text = strings.Join(textParts, "\n")
	}
	if text == "" {
		return "", fmt.Errorf("模型 %s 未返回 output_text", resolved.Ref)
	}
	emitModelOutput(onDelta, modelOutput{Text: text, Thinking: strings.Join(thinkingParts, "\n")})
	return text, nil
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
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

func anthropicMessages(ctx context.Context, resolved model.Resolved, client *http.Client, system, userText, mediaType string, image []byte, onDelta func(StreamDelta)) (string, error) {
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
		Stream:    true,
	}
	raw, streamed, err := postJSONStream(ctx, resolved, client, "/messages", body, true, onDelta, anthropicDelta)
	if err != nil {
		return "", err
	}
	if streamed != "" {
		return streamed, nil
	}
	var response struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("模型 %s 返回了无效 JSON: %w", resolved.Ref, err)
	}
	var textParts, thinkingParts []string
	for _, content := range response.Content {
		if content.Type == "text" && content.Text != "" {
			textParts = append(textParts, content.Text)
		}
		if content.Type == "thinking" && content.Thinking != "" {
			thinkingParts = append(thinkingParts, content.Thinking)
		}
	}
	if len(textParts) == 0 {
		return "", fmt.Errorf("模型 %s 未返回文本内容", resolved.Ref)
	}
	text := strings.Join(textParts, "\n")
	emitModelOutput(onDelta, modelOutput{Text: text, Thinking: strings.Join(thinkingParts, "\n")})
	return text, nil
}

type sseDelta func(event string, data []byte) ([]StreamDelta, error)

func postJSONStream(ctx context.Context, resolved model.Resolved, client *http.Client, endpoint string, body any, anthropic bool, onDelta func(StreamDelta), extract sseDelta) ([]byte, string, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(resolved.BaseURL, "/")+endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, "", err
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
		return nil, "", err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		defer resp.Body.Close()
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		if readErr != nil {
			return nil, "", readErr
		}
		message := strings.TrimSpace(string(responseBody))
		if parsed := parseAPIError(responseBody); parsed != "" {
			message = parsed
		}
		return nil, "", fmt.Errorf("模型 %s 请求失败 (HTTP %d): %.300s", resolved.Ref, resp.StatusCode, message)
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		text, err := readSSEText(resp, resolved, onDelta, extract)
		return nil, text, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, "", err
	}
	if message := parseAPIError(raw); message != "" {
		return nil, "", fmt.Errorf("模型 %s 返回错误: %s", resolved.Ref, message)
	}
	return raw, "", nil
}

func readSSEText(resp *http.Response, resolved model.Resolved, onDelta func(StreamDelta), extract sseDelta) (string, error) {
	defer resp.Body.Close()
	var out strings.Builder
	err := scanSSE(resp.Body, func(event string, data []byte) error {
		if string(data) == "[DONE]" {
			return nil
		}
		deltas, err := extract(event, data)
		if err != nil {
			return fmt.Errorf("模型 %s 返回了无效流事件: %w", resolved.Ref, err)
		}
		for _, delta := range deltas {
			if delta.Text == "" {
				continue
			}
			if delta.Kind == StreamOutput {
				out.WriteString(delta.Text)
			}
			emitDelta(onDelta, delta)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if out.Len() == 0 {
		return "", fmt.Errorf("模型 %s 未返回文本内容", resolved.Ref)
	}
	return out.String(), nil
}

func scanSSE(r io.Reader, visit func(event string, data []byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var event string
	var data []string
	dispatch := func() error {
		if len(data) == 0 {
			event = ""
			return nil
		}
		err := visit(event, []byte(strings.Join(data, "\n")))
		event, data = "", nil
		return err
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return dispatch()
}

func chatDelta(_ string, data []byte) ([]StreamDelta, error) {
	if message := parseAPIError(data); message != "" {
		return nil, errors.New(message)
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
				Thinking         json.RawMessage `json:"thinking"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, err
	}
	if len(chunk.Choices) == 0 {
		return nil, nil
	}
	delta := chunk.Choices[0].Delta
	var deltas []StreamDelta
	if thinking := firstTextContent(delta.ReasoningContent, delta.Reasoning, delta.Thinking); thinking != "" {
		deltas = append(deltas, StreamDelta{Kind: StreamThinking, Text: thinking})
	}
	if text := textFromContent(delta.Content); text != "" {
		deltas = append(deltas, StreamDelta{Kind: StreamOutput, Text: text})
	}
	return deltas, nil
}

func responsesDelta(event string, data []byte) ([]StreamDelta, error) {
	if message := parseAPIError(data); message != "" {
		return nil, errors.New(message)
	}
	var chunk struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, err
	}
	if chunk.Type == "response.error" || chunk.Type == "response.failed" ||
		event == "response.error" || event == "response.failed" {
		return nil, errors.New("Responses API 流返回错误")
	}
	if chunk.Type == "response.output_text.delta" || event == "response.output_text.delta" {
		return []StreamDelta{{Kind: StreamOutput, Text: chunk.Delta}}, nil
	}
	if chunk.Type == "response.reasoning_summary_text.delta" ||
		chunk.Type == "response.reasoning_text.delta" ||
		event == "response.reasoning_summary_text.delta" ||
		event == "response.reasoning_text.delta" {
		return []StreamDelta{{Kind: StreamThinking, Text: chunk.Delta}}, nil
	}
	return nil, nil
}

func anthropicDelta(event string, data []byte) ([]StreamDelta, error) {
	if message := parseAPIError(data); message != "" {
		return nil, errors.New(message)
	}
	var chunk struct {
		Type  string `json:"type"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, err
	}
	if chunk.Type == "error" || event == "error" {
		return nil, errors.New("Anthropic 流返回错误")
	}
	if chunk.Type == "content_block_delta" {
		switch chunk.Delta.Type {
		case "text_delta":
			return []StreamDelta{{Kind: StreamOutput, Text: chunk.Delta.Text}}, nil
		case "thinking_delta":
			return []StreamDelta{{Kind: StreamThinking, Text: chunk.Delta.Thinking}}, nil
		}
	}
	return nil, nil
}

func parseAPIError(body []byte) string {
	var parsed struct {
		Type     string          `json:"type"`
		Message  string          `json:"message"`
		Error    json.RawMessage `json:"error"`
		Response *struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	if len(parsed.Error) > 0 && string(parsed.Error) != "null" {
		var detail struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(parsed.Error, &detail) == nil && detail.Message != "" {
			return detail.Message
		}
		var message string
		if json.Unmarshal(parsed.Error, &message) == nil && message != "" {
			return message
		}
	}
	if parsed.Response != nil && parsed.Response.Error != nil && parsed.Response.Error.Message != "" {
		return parsed.Response.Error.Message
	}
	if (parsed.Type == "error" || strings.HasSuffix(parsed.Type, ".failed")) && parsed.Message != "" {
		return parsed.Message
	}
	return ""
}

func emitDelta(onDelta func(StreamDelta), delta StreamDelta) {
	if onDelta != nil && delta.Text != "" {
		onDelta(delta)
	}
}

func emitModelOutput(onDelta func(StreamDelta), output modelOutput) {
	emitDelta(onDelta, StreamDelta{Kind: StreamThinking, Text: output.Thinking})
	emitDelta(onDelta, StreamDelta{Kind: StreamOutput, Text: output.Text})
}

func firstTextContent(values ...json.RawMessage) string {
	for _, value := range values {
		if text := textFromContent(value); text != "" {
			return text
		}
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
