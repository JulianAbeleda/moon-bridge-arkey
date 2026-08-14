package chatingress

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"moonbridge/internal/format"
	"moonbridge/internal/protocol/chat"
)

func testAdapter() *Adapter {
	adapter := NewAdapter(format.CorePluginHooks{})
	adapter.now = func() time.Time { return time.Unix(1700000000, 0) }
	return adapter
}

func decodeRequest(t *testing.T, body string) *Request {
	t.Helper()
	var request Request
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return &request
}

// ============================================================================
// ToCoreRequest
// ============================================================================

func TestToCoreRequestMapsRolesAndSampling(t *testing.T) {
	request := decodeRequest(t, `{
		"model": "arkey-moonbridge",
		"max_tokens": 512,
		"temperature": 0.4,
		"stop": "END",
		"messages": [
			{"role": "system", "content": "be terse"},
			{"role": "user", "content": "hello"}
		]
	}`)

	coreReq, err := testAdapter().ToCoreRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("ToCoreRequest: %v", err)
	}
	if coreReq.Model != "arkey-moonbridge" {
		t.Errorf("Model = %q", coreReq.Model)
	}
	if coreReq.MaxTokens != 512 {
		t.Errorf("MaxTokens = %d, want 512 (max_tokens must be honoured)", coreReq.MaxTokens)
	}
	if coreReq.Temperature == nil || *coreReq.Temperature != 0.4 {
		t.Errorf("Temperature = %v", coreReq.Temperature)
	}
	if len(coreReq.StopSequences) != 1 || coreReq.StopSequences[0] != "END" {
		t.Errorf("StopSequences = %v, want [END] (scalar stop must decode)", coreReq.StopSequences)
	}
	if len(coreReq.System) != 1 || coreReq.System[0].Text != "be terse" {
		t.Errorf("System = %+v", coreReq.System)
	}
	if len(coreReq.Messages) != 1 || coreReq.Messages[0].Role != "user" {
		t.Fatalf("Messages = %+v", coreReq.Messages)
	}
	if coreReq.Messages[0].Content[0].Text != "hello" {
		t.Errorf("user text = %q", coreReq.Messages[0].Content[0].Text)
	}
}

func TestToCoreRequestPrefersMaxCompletionTokens(t *testing.T) {
	request := decodeRequest(t, `{
		"model": "m",
		"max_tokens": 100,
		"max_completion_tokens": 200,
		"messages": [{"role": "user", "content": "hi"}]
	}`)

	coreReq, err := testAdapter().ToCoreRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("ToCoreRequest: %v", err)
	}
	if coreReq.MaxTokens != 200 {
		t.Errorf("MaxTokens = %d, want 200", coreReq.MaxTokens)
	}
}

func TestToCoreRequestConvertsToolCallTurn(t *testing.T) {
	request := decodeRequest(t, `{
		"model": "m",
		"messages": [
			{"role": "user", "content": "list files"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "ls", "arguments": "{\"path\":\".\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "a.go\nb.go"}
		],
		"tools": [
			{"type": "function", "function": {"name": "ls", "description": "list", "parameters": {"type": "object"}}}
		],
		"tool_choice": "auto"
	}`)

	coreReq, err := testAdapter().ToCoreRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("ToCoreRequest: %v", err)
	}
	if len(coreReq.Messages) != 3 {
		t.Fatalf("len(Messages) = %d, want 3", len(coreReq.Messages))
	}

	assistant := coreReq.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 1 {
		t.Fatalf("assistant message = %+v", assistant)
	}
	block := assistant.Content[0]
	if block.Type != "tool_use" || block.ToolUseID != "call_1" || block.ToolName != "ls" {
		t.Errorf("tool_use block = %+v", block)
	}
	if string(block.ToolInput) != `{"path":"."}` {
		t.Errorf("ToolInput = %s, want unquoted JSON object", block.ToolInput)
	}

	toolResult := coreReq.Messages[2]
	if toolResult.Role != "tool" || toolResult.Content[0].Type != "tool_result" {
		t.Fatalf("tool message = %+v", toolResult)
	}
	if toolResult.Content[0].ToolUseID != "call_1" {
		t.Errorf("tool_result id = %q", toolResult.Content[0].ToolUseID)
	}

	if len(coreReq.Tools) != 1 || coreReq.Tools[0].Name != "ls" {
		t.Errorf("Tools = %+v", coreReq.Tools)
	}
	if coreReq.ToolChoice == nil || coreReq.ToolChoice.Mode != "auto" {
		t.Errorf("ToolChoice = %+v", coreReq.ToolChoice)
	}
}

func TestToCoreRequestNamedToolChoice(t *testing.T) {
	request := decodeRequest(t, `{
		"model": "m",
		"messages": [{"role": "user", "content": "hi"}],
		"tool_choice": {"type": "function", "function": {"name": "ls"}}
	}`)

	coreReq, err := testAdapter().ToCoreRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("ToCoreRequest: %v", err)
	}
	if coreReq.ToolChoice == nil || coreReq.ToolChoice.Name != "ls" {
		t.Fatalf("ToolChoice = %+v", coreReq.ToolChoice)
	}
}

func TestToCoreRequestConvertsInlineImage(t *testing.T) {
	request := decodeRequest(t, `{
		"model": "m",
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "what is this"},
			{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}
		]}]
	}`)

	coreReq, err := testAdapter().ToCoreRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("ToCoreRequest: %v", err)
	}
	blocks := coreReq.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("len(blocks) = %d, want 2", len(blocks))
	}
	if blocks[1].Type != "image" || blocks[1].MediaType != "image/png" || blocks[1].ImageData != "AAAA" {
		t.Errorf("image block = %+v", blocks[1])
	}
}

func TestToCoreRequestRejectsEmptyMessages(t *testing.T) {
	if _, err := testAdapter().ToCoreRequest(context.Background(), &Request{Model: "m"}); err == nil {
		t.Fatal("ToCoreRequest() error = nil, want error for empty messages")
	}
}

// ============================================================================
// FromCoreResponse
// ============================================================================

func TestFromCoreResponseText(t *testing.T) {
	coreResp := &format.CoreResponse{
		ID:     "msg_1",
		Status: "completed",
		Model:  "claude",
		Messages: []format.CoreMessage{{
			Role:    "assistant",
			Content: []format.CoreContentBlock{{Type: "text", Text: "hello"}},
		}},
		Usage:      format.CoreUsage{InputTokens: 10, OutputTokens: 3},
		StopReason: "end_turn",
	}

	out, err := testAdapter().FromCoreResponse(context.Background(), coreResp)
	if err != nil {
		t.Fatalf("FromCoreResponse: %v", err)
	}
	response, ok := out.(*chat.ChatResponse)
	if !ok {
		t.Fatalf("output type = %T", out)
	}
	if response.Object != "chat.completion" || len(response.Choices) != 1 {
		t.Fatalf("response = %+v", response)
	}
	if response.Choices[0].Message.Content != "hello" {
		t.Errorf("content = %v", response.Choices[0].Message.Content)
	}
	if response.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", response.Choices[0].FinishReason)
	}
	if response.Usage == nil || response.Usage.TotalTokens != 13 {
		t.Errorf("usage = %+v", response.Usage)
	}
}

func TestFromCoreResponseToolCallsUseStringArguments(t *testing.T) {
	coreResp := &format.CoreResponse{
		ID:     "msg_2",
		Status: "completed",
		Messages: []format.CoreMessage{{
			Role: "assistant",
			Content: []format.CoreContentBlock{{
				Type:      "tool_use",
				ToolUseID: "call_9",
				ToolName:  "ls",
				ToolInput: json.RawMessage(`{"path":"."}`),
			}},
		}},
		StopReason: "tool_use",
	}

	out, err := testAdapter().FromCoreResponse(context.Background(), coreResp)
	if err != nil {
		t.Fatalf("FromCoreResponse: %v", err)
	}
	response := out.(*chat.ChatResponse)
	calls := response.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].ID != "call_9" || calls[0].Function.Name != "ls" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if string(calls[0].Function.Arguments) != `"{\"path\":\".\"}"` {
		t.Errorf("arguments = %s, want a JSON string", calls[0].Function.Arguments)
	}
	if response.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", response.Choices[0].FinishReason)
	}

	// The whole response must round-trip through encoding/json unchanged.
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if !strings.Contains(string(encoded), `"arguments":"{\"path\":\".\"}"`) {
		t.Errorf("encoded response = %s", encoded)
	}
}

// ============================================================================
// FromCoreStream
// ============================================================================

func collectFrames(t *testing.T, adapter *Adapter, coreReq *format.CoreRequest, events []format.CoreStreamEvent) []format.ClientStreamFrame {
	t.Helper()
	source := make(chan format.CoreStreamEvent, len(events))
	for _, event := range events {
		source <- event
	}
	close(source)

	result, err := adapter.FromCoreStream(context.Background(), coreReq, source)
	if err != nil {
		t.Fatalf("FromCoreStream: %v", err)
	}
	frames, ok := result.(format.ClientStreamFrames)
	if !ok {
		t.Fatalf("result type = %T, want format.ClientStreamFrames", result)
	}
	collected := make([]format.ClientStreamFrame, 0, 8)
	for frame := range frames.Frames() {
		collected = append(collected, frame)
	}
	return collected
}

func TestFromCoreStreamTextChunks(t *testing.T) {
	coreReq := &format.CoreRequest{
		Model:      "m",
		Extensions: map[string]any{"openai_chat": map[string]any{"include_usage": true}},
	}
	frames := collectFrames(t, testAdapter(), coreReq, []format.CoreStreamEvent{
		{Type: format.CoreEventCreated, ItemID: "msg_1", Model: "m"},
		{Type: format.CoreContentBlockStarted, Index: 0, ContentBlock: &format.CoreContentBlock{Type: "text"}},
		{Type: format.CoreTextDelta, Index: 0, Delta: "he"},
		{Type: format.CoreTextDelta, Index: 0, Delta: "llo"},
		{Type: format.CoreEventCompleted, StopReason: "end_turn", Usage: &format.CoreUsage{InputTokens: 7, OutputTokens: 2}},
	})

	if len(frames) < 5 {
		t.Fatalf("frames = %d, want at least 5: %+v", len(frames), frames)
	}
	for _, frame := range frames {
		if frame.Event != "" {
			t.Errorf("frame carries an event name %q; Chat Completions frames are data-only", frame.Event)
		}
	}

	first := frames[0].Data.(StreamChunk)
	if first.Object != "chat.completion.chunk" || first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("first chunk = %+v", first)
	}
	if first.ID != "chatcmpl-msg_1" {
		t.Errorf("id = %q", first.ID)
	}

	var text strings.Builder
	var finish string
	var usage *chat.Usage
	for _, frame := range frames {
		chunk, ok := frame.Data.(StreamChunk)
		if !ok {
			continue
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			text.WriteString(choice.Delta.Content)
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
		}
	}
	if text.String() != "hello" {
		t.Errorf("streamed text = %q", text.String())
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q", finish)
	}
	if usage == nil || usage.PromptTokens != 7 || usage.CompletionTokens != 2 {
		t.Errorf("usage chunk = %+v", usage)
	}

	last := frames[len(frames)-1]
	if last.Raw != "[DONE]" {
		t.Errorf("last frame = %+v, want the [DONE] sentinel", last)
	}
}

func TestFromCoreStreamToolCallChunks(t *testing.T) {
	frames := collectFrames(t, testAdapter(), &format.CoreRequest{Model: "m"}, []format.CoreStreamEvent{
		{Type: format.CoreEventCreated, ItemID: "msg_2"},
		{Type: format.CoreContentBlockStarted, Index: 0, ContentBlock: &format.CoreContentBlock{
			Type: "tool_use", ToolUseID: "call_1", ToolName: "ls",
		}},
		{Type: format.CoreToolCallArgsDelta, Index: 0, Delta: `{"path"`},
		{Type: format.CoreToolCallArgsDelta, Index: 0, Delta: `:"."}`},
		{Type: format.CoreEventCompleted, StopReason: "tool_use"},
	})

	var name, arguments, finish string
	for _, frame := range frames {
		chunk, ok := frame.Data.(StreamChunk)
		if !ok {
			continue
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
			for _, call := range choice.Delta.ToolCalls {
				if call.Index != 0 {
					t.Errorf("tool call index = %v, want 0", call.Index)
				}
				if call.Function.Name != "" {
					name = call.Function.Name
				}
				arguments += call.Function.Arguments
			}
		}
	}
	if name != "ls" {
		t.Errorf("tool name = %q", name)
	}
	if arguments != `{"path":"."}` {
		t.Errorf("assembled arguments = %q", arguments)
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q", finish)
	}
}

func TestFromCoreStreamOmitsUsageChunkWithoutStreamOptions(t *testing.T) {
	frames := collectFrames(t, testAdapter(), &format.CoreRequest{Model: "m"}, []format.CoreStreamEvent{
		{Type: format.CoreEventCreated},
		{Type: format.CoreTextDelta, Index: 0, Delta: "hi"},
		{Type: format.CoreEventCompleted, Usage: &format.CoreUsage{InputTokens: 1, OutputTokens: 1}},
	})
	for _, frame := range frames {
		if chunk, ok := frame.Data.(StreamChunk); ok && chunk.Usage != nil {
			t.Fatalf("usage chunk emitted without stream_options.include_usage: %+v", chunk)
		}
	}
}

// Continuation chunks must not restate id/type/name: clients that assign
// rather than concatenate would blank out the tool call identity.
func TestFromCoreStreamContinuationChunksCarryOnlyIndexAndArguments(t *testing.T) {
	frames := collectFrames(t, testAdapter(), &format.CoreRequest{Model: "m"}, []format.CoreStreamEvent{
		{Type: format.CoreEventCreated},
		{Type: format.CoreContentBlockStarted, Index: 0, ContentBlock: &format.CoreContentBlock{
			Type: "tool_use", ToolUseID: "call_1", ToolName: "ls",
		}},
		{Type: format.CoreToolCallArgsDelta, Index: 0, Delta: `{"path":"."}`},
		{Type: format.CoreEventCompleted, StopReason: "tool_use"},
	})

	var encoded []string
	for _, frame := range frames {
		if frame.Data == nil {
			continue
		}
		data, err := json.Marshal(frame.Data)
		if err != nil {
			t.Fatalf("marshal frame: %v", err)
		}
		if chunk, ok := frame.Data.(StreamChunk); ok &&
			len(chunk.Choices) == 1 && len(chunk.Choices[0].Delta.ToolCalls) > 0 {
			encoded = append(encoded, string(data))
		}
	}
	if len(encoded) != 2 {
		t.Fatalf("tool_call chunks = %d, want 2: %v", len(encoded), encoded)
	}
	if !strings.Contains(encoded[0], `"id":"call_1"`) || !strings.Contains(encoded[0], `"name":"ls"`) {
		t.Errorf("opening chunk must carry id and name: %s", encoded[0])
	}
	for _, forbidden := range []string{`"id":""`, `"type":""`, `"name":""`} {
		if strings.Contains(encoded[1], forbidden) {
			t.Errorf("continuation chunk contains %s: %s", forbidden, encoded[1])
		}
	}
	if !strings.Contains(encoded[1], `"arguments":"{\"path\":\".\"}"`) {
		t.Errorf("continuation chunk arguments = %s", encoded[1])
	}
}

// Non-final chunks must carry an explicit null finish_reason.
func TestFromCoreStreamFinishReasonIsNullUntilTheEnd(t *testing.T) {
	frames := collectFrames(t, testAdapter(), &format.CoreRequest{Model: "m"}, []format.CoreStreamEvent{
		{Type: format.CoreEventCreated},
		{Type: format.CoreTextDelta, Index: 0, Delta: "hi"},
		{Type: format.CoreEventCompleted, StopReason: "end_turn"},
	})
	var chunks []string
	for _, frame := range frames {
		if chunk, ok := frame.Data.(StreamChunk); ok && len(chunk.Choices) > 0 {
			data, _ := json.Marshal(chunk)
			chunks = append(chunks, string(data))
		}
	}
	if len(chunks) < 2 {
		t.Fatalf("chunks = %v", chunks)
	}
	if !strings.Contains(chunks[0], `"finish_reason":null`) {
		t.Errorf("first chunk = %s, want an explicit null finish_reason", chunks[0])
	}
	last := chunks[len(chunks)-1]
	if !strings.Contains(last, `"finish_reason":"stop"`) {
		t.Errorf("last chunk = %s", last)
	}
}
