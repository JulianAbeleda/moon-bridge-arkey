package chatingress

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"moonbridge/internal/extension/codextool"
	"moonbridge/internal/format"
	"moonbridge/internal/protocol/chat"
)

// ClientProtocol is the inbound protocol identifier for this adapter.
// It matches config.ProtocolOpenAIChat; the registry keeps client and provider
// adapters in separate maps, so sharing the name with the upstream Chat
// provider adapter is intentional — it is the same wire protocol, inbound.
const ClientProtocol = "openai-chat"

// Adapter converts between inbound OpenAI Chat Completions DTOs and the Core
// intermediate format. It implements format.ClientAdapter and
// format.ClientStreamAdapter.
//
// The adapter is stateless; per-stream state lives in FromCoreStream.
type Adapter struct {
	hooks format.CorePluginHooks
	now   func() time.Time
}

// NewAdapter creates an Adapter with the given Core plugin hooks.
func NewAdapter(hooks format.CorePluginHooks) *Adapter {
	return &Adapter{hooks: hooks.WithDefaults(), now: time.Now}
}

// ClientProtocol returns the inbound protocol identifier.
func (a *Adapter) ClientProtocol() string { return ClientProtocol }

// ============================================================================
// ToCoreRequest — Chat Completions request → CoreRequest
// ============================================================================

// ToCoreRequest converts an inbound Chat Completions request into a CoreRequest.
//
// Mappings:
//   - system/developer messages → CoreRequest.System (in order)
//   - user/assistant messages   → CoreMessage with text/image/tool_use blocks
//   - tool messages             → role "tool" CoreMessage with a tool_result block
//   - tools                     → CoreTool (function name/description/parameters)
//   - tool_choice               → CoreToolChoice with the raw form preserved
//   - reasoning_effort          → CoreRequest.Output.Effort
func (a *Adapter) ToCoreRequest(ctx context.Context, req any) (*format.CoreRequest, error) {
	chatReq, err := asRequest(req)
	if err != nil {
		return nil, err
	}
	if len(chatReq.Messages) == 0 {
		return nil, fmt.Errorf("messages must not be empty")
	}

	coreReq := &format.CoreRequest{
		Model:         chatReq.Model,
		Temperature:   chatReq.Temperature,
		TopP:          chatReq.TopP,
		MaxTokens:     chatReq.EffectiveMaxTokens(),
		StopSequences: []string(chatReq.Stop),
		Stream:        chatReq.Stream,
		Metadata:      chatReq.Metadata,
	}

	system := make([]format.CoreContentBlock, 0, 1)
	messages := make([]format.CoreMessage, 0, len(chatReq.Messages))
	for i, message := range chatReq.Messages {
		switch message.Role {
		case "system", "developer":
			system = append(system, contentBlocks(message.Content)...)

		case "tool", "function":
			// Legacy `function` messages identify the call by name rather than
			// by tool_call_id.
			callID := message.ToolCallID
			if callID == "" {
				callID = message.Name
			}
			messages = append(messages, format.CoreMessage{
				Role: "tool",
				Content: []format.CoreContentBlock{{
					Type:              "tool_result",
					ToolUseID:         callID,
					ToolResultContent: contentBlocks(message.Content),
				}},
			})

		case "assistant":
			blocks := make([]format.CoreContentBlock, 0, len(message.ToolCalls)+1)
			if message.ReasoningContent != "" {
				blocks = append(blocks, format.CoreContentBlock{
					Type:          "reasoning",
					ReasoningText: message.ReasoningContent,
				})
			}
			blocks = append(blocks, contentBlocks(message.Content)...)
			for _, call := range message.ToolCalls {
				blocks = append(blocks, format.CoreContentBlock{
					Type:      "tool_use",
					ToolUseID: call.ID,
					ToolName:  call.Function.Name,
					ToolInput: decodeArguments(call.Function.Arguments),
				})
			}
			if len(blocks) == 0 {
				continue
			}
			messages = append(messages, format.CoreMessage{Role: "assistant", Content: blocks})

		case "user", "":
			blocks := contentBlocks(message.Content)
			if len(blocks) == 0 {
				continue
			}
			messages = append(messages, format.CoreMessage{Role: "user", Content: blocks})

		default:
			return nil, fmt.Errorf("messages[%d]: unsupported role %q", i, message.Role)
		}
	}
	coreReq.System = system
	coreReq.Messages = messages

	for _, tool := range chatReq.Tools {
		if tool.Function.Name == "" {
			continue
		}
		coreReq.Tools = append(coreReq.Tools, format.CoreTool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			InputSchema: tool.Function.Parameters,
		})
	}
	if injected := a.hooks.InjectTools(format.ContextWithCoreRequest(ctx, coreReq)); len(injected) > 0 {
		coreReq.Tools = append(coreReq.Tools, injected...)
	}

	if len(chatReq.ToolChoice) > 0 && string(chatReq.ToolChoice) != "null" {
		choice, err := convertToolChoice(chatReq.ToolChoice)
		if err != nil {
			return nil, fmt.Errorf("invalid tool_choice: %w", err)
		}
		coreReq.ToolChoice = choice
	}

	if chatReq.ReasoningEffort != "" {
		coreReq.Output = &format.CoreOutputConfig{Effort: chatReq.ReasoningEffort}
	}

	coreReq.Extensions = map[string]any{
		// Inbound Chat tools are always flat function tools, so the map is an
		// identity mapping — provider adapters still expect it to be present.
		"codex_tool_map": codextool.BuildToolMapFromCore(coreReq.Tools).Encode(),
	}
	chatExt := make(map[string]any)
	if chatReq.ParallelToolCalls != nil {
		chatExt["parallel_tool_calls"] = *chatReq.ParallelToolCalls
	}
	if chatReq.User != "" {
		chatExt["user"] = chatReq.User
	}
	if chatReq.IncludeUsage() {
		chatExt["include_usage"] = true
	}
	if len(chatExt) > 0 {
		coreReq.Extensions["openai_chat"] = chatExt
	}

	a.hooks.RewriteMessages(ctx, coreReq)
	a.hooks.MutateCoreRequest(ctx, coreReq)
	return coreReq, nil
}

// ============================================================================
// FromCoreResponse — CoreResponse → Chat Completions response
// ============================================================================

// FromCoreResponse converts a CoreResponse into a non-streaming Chat
// Completions response with a single choice.
func (a *Adapter) FromCoreResponse(ctx context.Context, resp *format.CoreResponse) (any, error) {
	if resp == nil {
		return nil, fmt.Errorf("core response is nil")
	}
	a.hooks.PostProcessCoreResponse(ctx, resp)

	message := chat.ChatMessage{Role: "assistant"}
	var text strings.Builder
	var reasoning strings.Builder
	for _, coreMessage := range resp.Messages {
		if coreMessage.Role != "assistant" {
			continue
		}
		for _, block := range coreMessage.Content {
			switch block.Type {
			case "text":
				text.WriteString(block.Text)
			case "reasoning":
				reasoning.WriteString(block.ReasoningText)
			case "image":
				text.WriteString("[Image]")
			case "tool_use":
				message.ToolCalls = append(message.ToolCalls, toolCallFromBlock(block, len(message.ToolCalls)))
			}
		}
	}
	if text.Len() > 0 {
		message.Content = text.String()
	}
	message.ReasoningContent = reasoning.String()

	response := &chat.ChatResponse{
		ID:      completionID(resp.ID),
		Object:  "chat.completion",
		Created: a.now().Unix(),
		Model:   resp.Model,
		Choices: []chat.Choice{{
			Index:        0,
			Message:      message,
			FinishReason: finishReason(resp.StopReason, len(message.ToolCalls) > 0),
		}},
		Usage: &chat.Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      totalTokens(resp.Usage),
		},
	}
	if resp.Usage.CachedInputTokens > 0 {
		response.Usage.PromptTokensDetails = &chat.PromptTokensDetails{CachedTokens: resp.Usage.CachedInputTokens}
	}
	return response, nil
}

// ============================================================================
// Helpers
// ============================================================================

func asRequest(req any) (*Request, error) {
	switch typed := req.(type) {
	case *Request:
		return typed, nil
	case Request:
		return &typed, nil
	default:
		return nil, fmt.Errorf("unexpected request type %T; expected *chatingress.Request", req)
	}
}

// contentBlocks converts a Chat message `content` field — a string, an array of
// content parts, or null — into Core content blocks.
func contentBlocks(content any) []format.CoreContentBlock {
	switch typed := content.(type) {
	case nil:
		return nil

	case string:
		if typed == "" {
			return nil
		}
		return []format.CoreContentBlock{{Type: "text", Text: typed}}

	case []format.CoreContentBlock:
		return typed

	case []any:
		blocks := make([]format.CoreContentBlock, 0, len(typed))
		for _, part := range typed {
			partMap, ok := part.(map[string]any)
			if !ok {
				if text, isString := part.(string); isString && text != "" {
					blocks = append(blocks, format.CoreContentBlock{Type: "text", Text: text})
				}
				continue
			}
			if block, ok := contentPartBlock(partMap); ok {
				blocks = append(blocks, block)
			}
		}
		return blocks

	default:
		// Anything else (numbers, bools) is stringified rather than dropped, so
		// a malformed client still gets its content across.
		encoded, err := json.Marshal(typed)
		if err != nil {
			return nil
		}
		return []format.CoreContentBlock{{Type: "text", Text: string(encoded)}}
	}
}

func contentPartBlock(part map[string]any) (format.CoreContentBlock, bool) {
	partType, _ := part["type"].(string)
	switch partType {
	case "text", "input_text", "output_text", "":
		text, _ := part["text"].(string)
		if text == "" {
			return format.CoreContentBlock{}, false
		}
		return format.CoreContentBlock{Type: "text", Text: text}, true

	case "image_url", "input_image", "image":
		url := ""
		if nested, ok := part["image_url"].(map[string]any); ok {
			url, _ = nested["url"].(string)
		} else if direct, ok := part["image_url"].(string); ok {
			url = direct
		}
		if url == "" {
			url, _ = part["image"].(string)
		}
		if url == "" {
			return format.CoreContentBlock{}, false
		}
		if mediaType, data, ok := parseDataURL(url); ok {
			return format.CoreContentBlock{Type: "image", MediaType: mediaType, ImageData: data}, true
		}
		// Remote images cannot be inlined into protocols that require base64
		// payloads; keep the reference visible to the model instead of dropping it.
		return format.CoreContentBlock{Type: "text", Text: "[Image: " + url + "]"}, true

	default:
		return format.CoreContentBlock{}, false
	}
}

// parseDataURL splits a data: URL into its media type and base64 payload.
func parseDataURL(url string) (mediaType string, data string, ok bool) {
	if !strings.HasPrefix(url, "data:") {
		return "", "", false
	}
	separator := strings.Index(url, ";base64,")
	if separator < 0 {
		return "", "", false
	}
	return url[len("data:"):separator], url[separator+len(";base64,"):], true
}

// decodeArguments normalizes tool call arguments to a raw JSON object.
// The Chat wire format carries them as a JSON-encoded string.
func decodeArguments(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage("{}")
	}
	if trimmed[0] != '"' {
		return json.RawMessage(trimmed)
	}
	var unquoted string
	if err := json.Unmarshal([]byte(trimmed), &unquoted); err != nil {
		return json.RawMessage(trimmed)
	}
	trimmedInner := strings.TrimSpace(unquoted)
	if trimmedInner == "" || !json.Valid([]byte(trimmedInner)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(trimmedInner)
}

// encodeArguments renders a raw JSON object as the JSON string the Chat wire
// format requires.
func encodeArguments(input json.RawMessage) json.RawMessage {
	arguments := strings.TrimSpace(string(input))
	if arguments == "" || arguments == "null" {
		arguments = "{}"
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return json.RawMessage(`"{}"`)
	}
	return encoded
}

func toolCallFromBlock(block format.CoreContentBlock, index int) chat.ToolCall {
	id := block.ToolUseID
	if id == "" {
		id = fmt.Sprintf("call_%d", index)
	}
	return chat.ToolCall{
		ID:   id,
		Type: "function",
		Function: chat.ToolCallFunc{
			Name:      block.ToolName,
			Arguments: encodeArguments(block.ToolInput),
		},
	}
}

// convertToolChoice maps the Chat Completions tool_choice shapes onto Core.
func convertToolChoice(raw json.RawMessage) (*format.CoreToolChoice, error) {
	choice := &format.CoreToolChoice{Raw: append(json.RawMessage(nil), raw...)}

	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch mode {
		case "auto", "none", "required":
			choice.Mode = mode
		case "any":
			choice.Mode = "required"
		default:
			return nil, fmt.Errorf("unknown tool_choice %q", mode)
		}
		return choice, nil
	}

	var object struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("tool_choice must be a string or an object")
	}
	name := object.Function.Name
	if name == "" {
		name = object.Name
	}
	if name == "" {
		return nil, fmt.Errorf("tool_choice object must name a function")
	}
	choice.Mode = object.Type
	if choice.Mode == "" {
		choice.Mode = "function"
	}
	choice.Name = name
	return choice, nil
}

// finishReason maps a Core stop reason onto the Chat Completions vocabulary.
func finishReason(stopReason string, hasToolCalls bool) string {
	switch stopReason {
	case "tool_use", "tool_calls":
		return "tool_calls"
	case "max_tokens", "length":
		return "length"
	case "content_filter":
		return "content_filter"
	case "end_turn", "stop_sequence", "stop", "":
		if hasToolCalls {
			return "tool_calls"
		}
		return "stop"
	default:
		return stopReason
	}
}

func totalTokens(usage format.CoreUsage) int {
	if usage.TotalTokens > 0 {
		return usage.TotalTokens
	}
	return usage.InputTokens + usage.OutputTokens
}

// completionID derives a chatcmpl-prefixed id from the upstream response id.
func completionID(id string) string {
	if id == "" {
		return "chatcmpl-moonbridge"
	}
	if strings.HasPrefix(id, "chatcmpl-") {
		return id
	}
	return "chatcmpl-" + id
}
