package client

// Anthropic Messages API support, implemented directly on net/http (no SDK).
//
// Two directions live in this file:
//
//  1. AnthropicClient — an upstream backend speaking the Anthropic Messages
//     API (/v1/messages). Internal OpenAI-format requests are translated to
//     Anthropic format on the way out and back to OpenAI format on the way
//     in, like every other Client.
//  2. AnthropicRequest / ChatResponse conversion helpers (AnthropicRequestToChat,
//     ChatResponseToAnthropic) shared with the server's /api/anthropic/*
//     endpoints, which expose *any* backend through the Anthropic wire format
//     so clients like Claude Code can talk to cllama directly.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// compile-time interface check
var _ Client = (*AnthropicClient)(nil)

// anthropicVersion is the value sent in the anthropic-version header.
const anthropicVersion = "2023-06-01"

// defaultMaxTokens is used when the caller did not set max_tokens; the
// Messages API always requires one.
const defaultMaxTokens = 8192

// ── Anthropic wire types ─────────────────────────────────────────────────────

// AnthropicImageSource is the base64 image source of an image block.
type AnthropicImageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// AnthropicBlock is one content block of a message (request or response).
// The meaning of each field depends on Type. Marshalling is customized so
// the tool_use "id" field round-trips.
type AnthropicBlock struct {
	Type string `json:"type"` // text | image | tool_use | tool_result | thinking

	Text     string `json:"text,omitempty"`     // text
	Thinking string `json:"thinking,omitempty"` // thinking

	ID   string          `json:"id,omitempty"`  // tool_use
	Name string          `json:"name,omitempty"` // tool_use
	Input json.RawMessage `json:"input,omitempty"` // tool_use

	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result: string | []AnthropicBlock
	IsError   bool            `json:"is_error,omitempty"`    // tool_result

	Source *AnthropicImageSource `json:"source,omitempty"` // image
}

// MarshalJSON / UnmarshalJSON keep the block shape explicit so each field
// only appears for the block types that use it.
func (b AnthropicBlock) MarshalJSON() ([]byte, error) {
	type alias AnthropicBlock
	return json.Marshal((*alias)(&b))
}

// UnmarshalJSON decodes a block.
func (b *AnthropicBlock) UnmarshalJSON(data []byte) error {
	type alias AnthropicBlock
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*b = AnthropicBlock(a)
	return nil
}

// AnthropicMessage is one message of an Anthropic request.
type AnthropicMessage struct {
	Role    string          `json:"role"`    // user | assistant
	Content json.RawMessage `json:"content"` // string | []AnthropicBlock
}

// AnthropicToolDef is a tool definition in Anthropic format.
type AnthropicToolDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

// AnthropicRequest is the /v1/messages request body.
type AnthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"` // string | []AnthropicBlock
	Messages      []AnthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	Tools         []AnthropicToolDef `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	Thinking      json.RawMessage    `json:"thinking,omitempty"`
}

// AnthropicUsage reports token usage in Anthropic format.
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// AnthropicResponse is a /v1/messages response body (a message object).
type AnthropicResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"` // "message"
	Role         string           `json:"role"` // "assistant"
	Model        string           `json:"model"`
	Content      []AnthropicBlock `json:"content"`
	StopReason   string           `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        AnthropicUsage   `json:"usage"`
}

// ── Content helpers ──────────────────────────────────────────────────────────

// decodeAnthropicContent normalizes a message content field, which may be a
// bare string or an array of content blocks, into a block list.
func decodeAnthropicContent(raw json.RawMessage) ([]AnthropicBlock, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []AnthropicBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []AnthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

// blocksText joins the text of all text blocks with newlines.
func blocksText(blocks []AnthropicBlock) string {
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// ── Anthropic request → internal ChatRequest ─────────────────────────────────

// AnthropicRequestToChat translates an Anthropic Messages request into the
// internal OpenAI-format request the proxy pipeline runs on. The Stream flag
// is left to the caller (the endpoint handler decides based on its own
// routing).
func AnthropicRequestToChat(ar *AnthropicRequest) (*ChatRequest, error) {
	req := &ChatRequest{
		Model:       ar.Model,
		MaxTokens:   ar.MaxTokens,
		Temperature: ar.Temperature,
		Stop:        ar.StopSequences,
	}

	// system: string or text-block array → leading system message.
	if len(ar.System) > 0 {
		sysBlocks, err := decodeAnthropicContent(ar.System)
		if err != nil {
			return nil, fmt.Errorf("invalid system: %w", err)
		}
		if sys := blocksText(sysBlocks); sys != "" {
			req.Messages = append(req.Messages, NewTextMessage("system", sys))
		}
	}

	for i, m := range ar.Messages {
		blocks, err := decodeAnthropicContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("invalid content in message %d: %w", i, err)
		}
		switch m.Role {
		case "user":
			req.Messages = append(req.Messages, userBlocksToChatMessages(blocks)...)
		case "assistant":
			req.Messages = append(req.Messages, assistantBlocksToChatMessage(blocks))
		default:
			return nil, fmt.Errorf("unsupported role %q", m.Role)
		}
	}

	for _, t := range ar.Tools {
		req.Tools = append(req.Tools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	if len(ar.ToolChoice) > 0 {
		tc, err := anthropicToolChoiceToOpenAI(ar.ToolChoice)
		if err != nil {
			return nil, err
		}
		req.ToolChoice = tc
	}

	// thinking: {"type":"enabled",...} → the internal thinking toggle; the
	// client layer turns it back into a budget for Anthropic upstreams.
	if len(ar.Thinking) > 0 {
		var th struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(ar.Thinking, &th); err != nil {
			return nil, fmt.Errorf("invalid thinking: %w", err)
		}
		if th.Type == "enabled" {
			req.Think = true
		}
	}

	return req, nil
}

// userBlocksToChatMessages translates Anthropic user-message blocks to OpenAI
// messages. tool_result blocks become role "tool" messages (as OpenAI
// expects); text and image blocks accumulate into a single user message,
// flushed in order relative to the tool results.
func userBlocksToChatMessages(blocks []AnthropicBlock) []ChatMessage {
	var out []ChatMessage
	var parts []MessagePart

	flush := func() {
		if len(parts) == 0 {
			return
		}
		var asImages bool
		for _, p := range parts {
			if p.Type == "image_url" {
				asImages = true
				break
			}
		}
		if !asImages {
			out = append(out, NewTextMessage("user", blocksText(partsToBlocks(parts))))
		} else {
			out = append(out, ChatMessage{Role: "user", Content: parts})
		}
		parts = nil
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, MessagePart{Type: "text", Text: b.Text})
			}
		case "image":
			if b.Source != nil && b.Source.Type == "base64" {
				parts = append(parts, MessagePart{
					Type:     "image_url",
					ImageURL: &ImageURL{URL: "data:" + b.Source.MediaType + ";base64," + b.Source.Data},
				})
			}
		case "tool_result":
			flush()
			out = append(out, ChatMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    toolResultText(b),
			})
		case "thinking", "tool_use":
			// Echoed assistant blocks inside a user turn: nothing to map.
		}
	}
	flush()
	if len(out) == 0 {
		out = append(out, NewTextMessage("user", ""))
	}
	return out
}

// toolResultText renders a tool_result block's content (string or nested
// text/image blocks) as a plain string for the OpenAI tool message. Errors
// are prefixed so downstream models see them as failures.
func toolResultText(b AnthropicBlock) string {
	raw := string(b.Content)
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		raw = s
	} else {
		var nested []AnthropicBlock
		if json.Unmarshal(b.Content, &nested) == nil {
			var texts []string
			for _, nb := range nested {
				switch nb.Type {
				case "text":
					texts = append(texts, nb.Text)
				case "image":
					texts = append(texts, "[tool result image omitted]")
				}
			}
			raw = strings.Join(texts, "\n")
		}
	}
	if b.IsError {
		raw = "Error: " + raw
	}
	return raw
}

// assistantBlocksToChatMessage translates Anthropic assistant-message blocks
// (text / tool_use / thinking) into one OpenAI-format message.
func assistantBlocksToChatMessage(blocks []AnthropicBlock) ChatMessage {
	msg := ChatMessage{Role: "assistant"}
	var texts, thinking []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "thinking":
			if b.Thinking != "" {
				thinking = append(thinking, b.Thinking)
			}
		case "tool_use":
			args := string(b.Input)
			if !json.Valid(b.Input) {
				args = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:       b.ID,
				Type:     "function",
				Function: ToolCallFunction{Name: b.Name, Arguments: args},
			})
		}
	}
	msg.Content = strings.Join(texts, "\n")
	msg.ReasoningContent = strings.Join(thinking, "\n")
	return msg
}

// anthropicToolChoiceToOpenAI maps Anthropic tool_choice to its OpenAI
// equivalent.
func anthropicToolChoiceToOpenAI(raw json.RawMessage) (interface{}, error) {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, fmt.Errorf("invalid tool_choice: %w", err)
	}
	switch tc.Type {
	case "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "none":
		return "none", nil
	case "tool":
		return map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": tc.Name},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported tool_choice type %q", tc.Type)
	}
}

// partsToBlocks adapts OpenAI message parts to text-only Anthropic blocks,
// the input blocksText needs for the text-only fast path.
func partsToBlocks(parts []MessagePart) []AnthropicBlock {
	blocks := make([]AnthropicBlock, 0, len(parts))
	for _, p := range parts {
		if p.Type == "text" {
			blocks = append(blocks, AnthropicBlock{Type: "text", Text: p.Text})
		}
	}
	return blocks
}

// ── Internal ChatResponse → Anthropic response ───────────────────────────────

// ChatResponseToAnthropic converts an internal OpenAI-format chat response
// into an Anthropic message object. requestedModel is echoed back as the
// model (Anthropic clients expect their own model name mirrored).
func ChatResponseToAnthropic(resp *ChatResponse, requestedModel string) *AnthropicResponse {
	out := &AnthropicResponse{
		ID:         newAnthropicID(),
		Type:       "message",
		Role:       "assistant",
		Model:      requestedModel,
		StopReason: "end_turn",
		Usage:      AnthropicUsage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens},
	}
	if len(resp.Choices) == 0 {
		return out
	}
	m := resp.Choices[0].Message
	if r := AnthropicStopReason(resp.Choices[0].FinishReason); r != "" {
		out.StopReason = r
	}
	if th := m.Reasoning(); th != "" {
		out.Content = append(out.Content, AnthropicBlock{Type: "thinking", Thinking: th})
	}
	if txt := chatResponseText(m.Content); txt != "" {
		out.Content = append(out.Content, AnthropicBlock{Type: "text", Text: txt})
	}
	for _, tc := range m.ToolCalls {
		input := json.RawMessage(tc.Function.Arguments)
		if !json.Valid(input) {
			input = json.RawMessage("{}")
		}
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("toolu_%d", time.Now().UnixNano())
		}
		out.Content = append(out.Content, AnthropicBlock{
			Type:  "tool_use",
			ID:    id,
			Name:  tc.Function.Name,
			Input: input,
		})
	}
	return out
}

// chatResponseText flattens a message's content (string, text-part array or
// nil) into plain text.
func chatResponseText(content interface{}) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []MessagePart:
		return blocksText(partsToBlocks(c))
	case []interface{}:
		// Messages decoded from JSON carry []interface{} parts.
		var texts []string
		for _, raw := range c {
			if p, ok := raw.(map[string]interface{}); ok {
				if p["type"] == "text" {
					if t, ok := p["text"].(string); ok {
						texts = append(texts, t)
					}
				}
			}
		}
		return strings.Join(texts, "\n")
	default:
		return fmt.Sprintf("%v", content)
	}
}

// ── Anthropic response → internal ChatResponse ───────────────────────────────

// convertAnthropicChatResponse translates a non-streaming Anthropic message
// into the internal OpenAI-format response.
func convertAnthropicChatResponse(ar *AnthropicResponse, model string) *ChatResponse {
	msg := ChatMessage{Role: "assistant"}
	var texts, thinking []string
	for _, b := range ar.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "thinking":
			if b.Thinking != "" {
				thinking = append(thinking, b.Thinking)
			}
		case "tool_use":
			args := string(b.Input)
			if !json.Valid(b.Input) {
				args = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:       b.ID,
				Type:     "function",
				Function: ToolCallFunction{Name: b.Name, Arguments: args},
			})
		}
	}
	msg.Content = strings.Join(texts, "\n")
	msg.ReasoningContent = strings.Join(thinking, "\n")

	id := ar.ID
	if id == "" {
		id = newAnthropicID()
	}
	return &ChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: AnthropicFinishReason(ar.StopReason),
		}},
		Usage: ChatUsage{
			PromptTokens:     ar.Usage.InputTokens,
			CompletionTokens: ar.Usage.OutputTokens,
			TotalTokens:      ar.Usage.InputTokens + ar.Usage.OutputTokens,
		},
	}
}

// ── Stop-reason mapping ──────────────────────────────────────────────────────

// AnthropicFinishReason maps an Anthropic stop_reason to an OpenAI
// finish_reason ("stop" when unknown).
func AnthropicFinishReason(stop string) string {
	switch stop {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// AnthropicStopReason maps an OpenAI finish_reason to an Anthropic
// stop_reason.
func AnthropicStopReason(finish string) string {
	switch finish {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func newAnthropicID() string {
	return fmt.Sprintf("msg_%d", time.Now().UnixNano())
}

// ── AnthropicClient (upstream backend) ───────────────────────────────────────

// AnthropicClient talks to an Anthropic-compatible Messages API using
// net/http directly (api.anthropic.com, NewAPI gateways, …).
type AnthropicClient struct {
	BaseURL    string
	APIKey     string // sent as x-api-key
	HTTPClient *http.Client
}

// NewAnthropicClient creates a client for an Anthropic-compatible server.
// baseURL is the server root (e.g. https://api.anthropic.com); a trailing
// /v1 is tolerated. apiKey may be empty for unauthenticated servers. timeout
// bounds a whole request including body streaming (long generations need a
// generous value); 0 means no timeout.
func NewAnthropicClient(baseURL, apiKey string, timeout time.Duration) *AnthropicClient {
	return &AnthropicClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: timeout},
	}
}

// messagesPath returns the messages endpoint path, tolerating base URLs that
// already end in /v1.
func (c *AnthropicClient) messagesPath() string {
	if strings.HasSuffix(c.BaseURL, "/v1") {
		return "/messages"
	}
	return "/v1/messages"
}

// Chat sends a non-streaming chat request to the Anthropic API.
func (c *AnthropicClient) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	httpReq, err := c.buildHTTPRequest(ctx, c.buildRequest(req, false))
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic chat request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.readError(resp)
	}

	var anthResp AnthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&anthResp); err != nil {
		return nil, fmt.Errorf("anthropic chat decode: %w", err)
	}
	return convertAnthropicChatResponse(&anthResp, req.Model), nil
}

// ChatStream sends a streaming chat request and returns the raw SSE body.
func (c *AnthropicClient) ChatStream(ctx context.Context, req *ChatRequest) (io.ReadCloser, error) {
	httpReq, err := c.buildHTTPRequest(ctx, c.buildRequest(req, true))
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic stream request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.readError(resp)
	}
	return resp.Body, nil
}

// Embeddings always fails: the Anthropic Messages API has no embeddings
// endpoint.
func (c *AnthropicClient) Embeddings(ctx context.Context, model string, input interface{}) ([][]float64, error) {
	return nil, fmt.Errorf("anthropic upstream does not support embeddings")
}

// readError turns a non-2xx response into an error carrying Anthropic's
// error envelope when present.
func (c *AnthropicClient) readError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		return fmt.Errorf("anthropic HTTP %d: %s (%s)", resp.StatusCode, env.Error.Message, env.Error.Type)
	}
	return fmt.Errorf("anthropic HTTP %d: %s", resp.StatusCode, string(body))
}

// ── Request building ─────────────────────────────────────────────────────────

// buildRequest translates the internal OpenAI-format request into an
// Anthropic Messages request.
func (c *AnthropicClient) buildRequest(req *ChatRequest, stream bool) *AnthropicRequest {
	anth := &AnthropicRequest{
		Model:         req.Model,
		Stream:        stream,
		Temperature:   req.Temperature,
		StopSequences: req.Stop,
	}

	// Messages: system prompts move to the top-level system field; the rest
	// are re-typed per Anthropic's block format.
	var systemTexts []string
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			if txt := chatResponseText(m.Content); txt != "" {
				systemTexts = append(systemTexts, txt)
			}
		case "tool":
			anth.Messages = append(anth.Messages, anthropicToolResultMessage(m))
		default:
			anth.Messages = append(anth.Messages, anthropicMessageFromChat(m))
		}
	}
	if len(systemTexts) > 0 {
		sys, _ := json.Marshal(strings.Join(systemTexts, "\n\n"))
		anth.System = sys
	}

	for _, t := range req.Tools {
		schema := t.Function.Parameters
		if schema == nil {
			schema = map[string]interface{}{"type": "object"}
		}
		anth.Tools = append(anth.Tools, AnthropicToolDef{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	if len(anth.Tools) > 0 {
		anth.ToolChoice = openAIToolChoiceToAnthropic(req.ToolChoice)
	}

	// max_tokens is mandatory; extended thinking additionally requires a
	// budget below it.
	anth.MaxTokens = req.MaxTokens
	if anth.MaxTokens <= 0 {
		anth.MaxTokens = defaultMaxTokens
	}
	if budget := thinkingBudget(req); budget > 0 {
		anth.Thinking = json.RawMessage(fmt.Sprintf(`{"type":"enabled","budget_tokens":%d}`, budget))
		if anth.MaxTokens <= budget {
			anth.MaxTokens = budget + 2048
		}
	}
	return anth
}

// thinkingBudget resolves the internal thinking toggle/effort into an
// Anthropic budget_tokens value (0 = thinking off).
func thinkingBudget(req *ChatRequest) int {
	switch req.ThinkingEffort() {
	case "low":
		return 1024
	case "medium":
		return 4096
	case "high":
		return 16384
	case "":
		if b, ok := req.Think.(bool); ok && b {
			return 2048
		}
		return 0
	default:
		return 2048
	}
}

// anthropicToolResultMessage maps an OpenAI role "tool" message back to a
// user message carrying a tool_result block.
func anthropicToolResultMessage(m ChatMessage) AnthropicMessage {
	block := AnthropicBlock{
		Type:      "tool_result",
		ToolUseID: m.ToolCallID,
		Content:   mustJSONText(chatResponseText(m.Content)),
	}
	raw, _ := json.Marshal([]AnthropicBlock{block})
	return AnthropicMessage{Role: "user", Content: raw}
}

// anthropicMessageFromChat maps a user/assistant OpenAI message to Anthropic
// blocks. Assistant thinking traces are dropped: Anthropic rejects thinking
// blocks that lack a valid signature.
func anthropicMessageFromChat(m ChatMessage) AnthropicMessage {
	var blocks []AnthropicBlock

	switch c := m.Content.(type) {
	case nil:
	case string:
		if c != "" {
			blocks = append(blocks, AnthropicBlock{Type: "text", Text: c})
		}
	case []MessagePart:
		for _, p := range c {
			if b, ok := messagePartToAnthropic(p); ok {
				blocks = append(blocks, b)
			}
		}
	case []interface{}:
		for _, raw := range c {
			p, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			var part MessagePart
			if b, err := json.Marshal(p); err == nil && json.Unmarshal(b, &part) == nil {
				if blk, ok := messagePartToAnthropic(part); ok {
					blocks = append(blocks, blk)
				}
			}
		}
	default:
		if txt := fmt.Sprintf("%v", m.Content); txt != "" {
			blocks = append(blocks, AnthropicBlock{Type: "text", Text: txt})
		}
	}

	for _, tc := range m.ToolCalls {
		input := json.RawMessage(tc.Function.Arguments)
		if !json.Valid(input) {
			input = json.RawMessage("{}")
		}
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("toolu_%d", time.Now().UnixNano())
		}
		blocks = append(blocks, AnthropicBlock{Type: "tool_use", ID: id, Name: tc.Function.Name, Input: input})
	}

	if len(blocks) == 1 && blocks[0].Type == "text" {
		// Bare-string content shorthand.
		raw, _ := json.Marshal(blocks[0].Text)
		return AnthropicMessage{Role: m.Role, Content: raw}
	}
	raw, _ := json.Marshal(blocks)
	return AnthropicMessage{Role: m.Role, Content: raw}
}

// messagePartToAnthropic converts one OpenAI message part; returns false for
// parts with no Anthropic equivalent (e.g. remote image URLs).
func messagePartToAnthropic(p MessagePart) (AnthropicBlock, bool) {
	switch p.Type {
	case "text":
		if p.Text == "" {
			return AnthropicBlock{}, false
		}
		return AnthropicBlock{Type: "text", Text: p.Text}, true
	case "image_url":
		if p.ImageURL == nil {
			return AnthropicBlock{}, false
		}
		if b64, isData := dataURLBase64(p.ImageURL.URL); isData {
			mediaType := "image/png"
			if semi := strings.Index(p.ImageURL.URL, ";"); semi > 5 {
				mediaType = p.ImageURL.URL[5:semi]
			}
			return AnthropicBlock{Type: "image", Source: &AnthropicImageSource{
				Type: "base64", MediaType: mediaType, Data: b64,
			}}, true
		}
		return AnthropicBlock{Type: "text", Text: "[image: " + p.ImageURL.URL + "]"}, true
	default:
		return AnthropicBlock{}, false
	}
}

// openAIToolChoiceToAnthropic maps the OpenAI tool_choice values
// ("auto"/"required"/"none"/{"type":"function",...}) to Anthropic's.
func openAIToolChoiceToAnthropic(tc interface{}) json.RawMessage {
	switch v := tc.(type) {
	case nil:
		return nil
	case string:
		switch v {
		case "required":
			return json.RawMessage(`{"type":"any"}`)
		case "none":
			return json.RawMessage(`{"type":"none"}`)
		default:
			return json.RawMessage(`{"type":"auto"}`)
		}
	case map[string]interface{}:
		if fn, ok := v["function"].(map[string]interface{}); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				raw, _ := json.Marshal(map[string]string{"type": "tool", "name": name})
				return raw
			}
		}
	}
	return nil
}

// mustJSONText JSON-marshals s as a string, falling back to an empty one.
func mustJSONText(s string) json.RawMessage {
	raw, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return raw
}

// buildRequest helpers ────────────────────────────────────────────────────────

func (c *AnthropicClient) buildHTTPRequest(ctx context.Context, body any) (*http.Request, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+c.messagesPath(), bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	if c.APIKey != "" {
		req.Header.Set("x-api-key", c.APIKey)
	}
	return req, nil
}

// ── Streaming: Anthropic SSE → internal OpenAI SSE ───────────────────────────

// StreamToSSE translates Anthropic's SSE stream (message_start,
// content_block_*, message_delta, message_stop) into OpenAI-format SSE
// events. It closes out when done and returns an error if the stream ended
// before message_stop or carried an error event.
func (c *AnthropicClient) StreamToSSE(ctx context.Context, body io.Reader, model string, out chan<- string) error {
	defer close(out)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	id := "chatcmpl-" + fmt.Sprintf("%d", time.Now().UnixNano())

	var usage ChatUsage
	var finishReason string
	toolIDs := map[int]string{} // content block index → tool_use id

	emit := func(delta ChatMessage, finish string) error {
		evt := StreamingChatResponse{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   model,
		}
		if delta.Role != "" || delta.Content != nil || delta.ReasoningContent != "" || len(delta.ToolCalls) > 0 {
			evt.Choices = []StreamingChatChoice{{Index: 0, Delta: delta}}
		}
		if finish != "" {
			evt.Choices = append(evt.Choices, StreamingChatChoice{Index: 0, FinishReason: finish})
			evt.Usage = usage
		}
		if !writeSSEEvent(ctx, out, evt) {
			return ctx.Err()
		}
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}

		var evt struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			ContentBlock *AnthropicBlock `json:"content_block"`
			Message      *struct {
				Usage *AnthropicUsage `json:"usage"`
			} `json:"message"`
			Usage *AnthropicUsage `json:"usage"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue // ignore unparseable keep-alives
		}

		switch evt.Type {
		case "ping":
			// no-op
		case "error":
			msg := "upstream error"
			if evt.Error != nil && evt.Error.Message != "" {
				msg = evt.Error.Message
			}
			return fmt.Errorf("anthropic stream error: %s", msg)

		case "message_start":
			if evt.Message != nil && evt.Message.Usage != nil {
				usage.PromptTokens = evt.Message.Usage.InputTokens
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
			}
			if err := emit(ChatMessage{Role: "assistant"}, ""); err != nil {
				return err
			}

		case "content_block_start":
			if evt.ContentBlock != nil && evt.ContentBlock.Type == "tool_use" {
				toolIDs[evt.Index] = evt.ContentBlock.ID
				if err := emit(ChatMessage{ToolCalls: []ToolCall{{
					ID:       evt.ContentBlock.ID,
					Type:     "function",
					Function: ToolCallFunction{Name: evt.ContentBlock.Name},
				}}}, ""); err != nil {
					return err
				}
			}

		case "content_block_delta":
			switch evt.Delta.Type {
			case "text_delta":
				if evt.Delta.Text != "" {
					if err := emit(ChatMessage{Content: evt.Delta.Text}, ""); err != nil {
						return err
					}
				}
			case "thinking_delta":
				if evt.Delta.Thinking != "" {
					if err := emit(ChatMessage{ReasoningContent: evt.Delta.Thinking}, ""); err != nil {
						return err
					}
				}
			case "input_json_delta":
				if evt.Delta.PartialJSON != "" {
					if err := emit(ChatMessage{ToolCalls: []ToolCall{{
						ID:       toolIDs[evt.Index],
						Type:     "function",
						Function: ToolCallFunction{Arguments: evt.Delta.PartialJSON},
					}}}, ""); err != nil {
						return err
					}
				}
			}

		case "message_delta":
			if evt.Delta.StopReason != "" {
				finishReason = AnthropicFinishReason(evt.Delta.StopReason)
			}
			if evt.Usage != nil {
				usage.CompletionTokens = evt.Usage.OutputTokens
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
			}

		case "message_stop":
			if finishReason == "" {
				finishReason = "stop"
			}
			if err := emit(ChatMessage{}, finishReason); err != nil {
				return err
			}
			select {
			case out <- "data: [DONE]\n\n":
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("anthropic stream read: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("anthropic upstream closed before message_stop")
}
