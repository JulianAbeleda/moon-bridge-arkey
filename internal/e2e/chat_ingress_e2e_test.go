//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"moonbridge/internal/format"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/protocol/chat"
	"moonbridge/internal/protocol/chatingress"
)

// ============================================================================
// Inbound OpenAI Chat Completions → Anthropic upstream
//
// This is the path a Chat-Completions-only client (Crush, Cline, Aider) takes
// through MoonBridge: /v1/chat/completions → Core → Anthropic Messages, and
// back again.
// ============================================================================

func decodeChatRequest(t *testing.T, body string) *chatingress.Request {
	t.Helper()
	var request chatingress.Request
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatalf("decode chat request: %v", err)
	}
	return &request
}

func TestChatIngressE2E_TextRoundTrip(t *testing.T) {
	ctx := context.Background()
	cfg := e2eMinimalConfig()
	hooks := format.CorePluginHooks{}.WithDefaults()
	reg := newTestRegistry(t, cfg, hooks)

	client, ok := reg.GetClient(configOpenAIChat)
	if !ok {
		t.Fatal("chat ingress client adapter not found")
	}
	provider, ok := reg.GetProvider(configAnthropic)
	if !ok {
		t.Fatal("provider adapter not found")
	}

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "msg_chat_ingress_001",
			"type": "message",
			"role": "assistant",
			"content": [{"type": "text", "text": "Hello from Anthropic mock!"}],
			"model": "claude-3.5-sonnet-20241022",
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 11, "output_tokens": 4}
		}`)
	}))
	defer mockSrv.Close()

	// A Crush-shaped request: system message, max_tokens (not
	// max_completion_tokens), streaming off.
	chatReq := decodeChatRequest(t, `{
		"model": "claude-3.5-sonnet",
		"max_tokens": 128,
		"messages": [
			{"role": "system", "content": "be terse"},
			{"role": "user", "content": "Hello"}
		]
	}`)

	coreReq, err := client.ToCoreRequest(ctx, chatReq)
	if err != nil {
		t.Fatalf("ToCoreRequest: %v", err)
	}

	upstreamAny, err := provider.FromCoreRequest(ctx, coreReq)
	if err != nil {
		t.Fatalf("FromCoreRequest: %v", err)
	}
	upstreamReq := upstreamAny.(*anthropic.MessageRequest)
	if upstreamReq.MaxTokens != 128 {
		t.Errorf("upstream MaxTokens = %d, want 128", upstreamReq.MaxTokens)
	}
	if len(upstreamReq.System) == 0 {
		t.Error("system message did not survive conversion")
	}

	anthClient := anthropic.NewClient(anthropic.ClientConfig{
		BaseURL: mockSrv.URL,
		APIKey:  "test-key",
		Client:  mockSrv.Client(),
	})
	upstreamResp, err := anthClient.CreateMessage(ctx, *upstreamReq)
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	coreResp, err := provider.ToCoreResponse(ctx, &upstreamResp)
	if err != nil {
		t.Fatalf("ToCoreResponse: %v", err)
	}

	outAny, err := client.FromCoreResponse(ctx, coreResp)
	if err != nil {
		t.Fatalf("FromCoreResponse: %v", err)
	}
	response, ok := outAny.(*chat.ChatResponse)
	if !ok {
		t.Fatalf("output type = %T, want *chat.ChatResponse", outAny)
	}
	if response.Object != "chat.completion" {
		t.Errorf("object = %q", response.Object)
	}
	if len(response.Choices) != 1 || response.Choices[0].Message.Content != "Hello from Anthropic mock!" {
		t.Fatalf("choices = %+v", response.Choices)
	}
	if response.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", response.Choices[0].FinishReason)
	}
	if response.Usage == nil || response.Usage.PromptTokens != 11 || response.Usage.CompletionTokens != 4 {
		t.Errorf("usage = %+v", response.Usage)
	}
}

func TestChatIngressE2E_ToolCallRoundTrip(t *testing.T) {
	ctx := context.Background()
	cfg := e2eMinimalConfig()
	hooks := format.CorePluginHooks{}.WithDefaults()
	reg := newTestRegistry(t, cfg, hooks)

	client, _ := reg.GetClient(configOpenAIChat)
	provider, _ := reg.GetProvider(configAnthropic)

	var upstreamBody []byte
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "msg_chat_ingress_002",
			"type": "message",
			"role": "assistant",
			"content": [{"type": "tool_use", "id": "toolu_1", "name": "ls", "input": {"path": "."}}],
			"model": "claude-3.5-sonnet-20241022",
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 20, "output_tokens": 9}
		}`)
	}))
	defer mockSrv.Close()

	chatReq := decodeChatRequest(t, `{
		"model": "claude-3.5-sonnet",
		"max_tokens": 256,
		"tool_choice": "auto",
		"tools": [{"type": "function", "function": {
			"name": "ls", "description": "list files",
			"parameters": {"type": "object", "properties": {"path": {"type": "string"}}}
		}}],
		"messages": [
			{"role": "user", "content": "list files"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "toolu_0", "type": "function", "function": {"name": "ls", "arguments": "{\"path\":\"/tmp\"}"}}
			]},
			{"role": "tool", "tool_call_id": "toolu_0", "content": "nothing here"}
		]
	}`)

	coreReq, err := client.ToCoreRequest(ctx, chatReq)
	if err != nil {
		t.Fatalf("ToCoreRequest: %v", err)
	}
	upstreamAny, err := provider.FromCoreRequest(ctx, coreReq)
	if err != nil {
		t.Fatalf("FromCoreRequest: %v", err)
	}

	anthClient := anthropic.NewClient(anthropic.ClientConfig{
		BaseURL: mockSrv.URL,
		APIKey:  "test-key",
		Client:  mockSrv.Client(),
	})
	upstreamResp, err := anthClient.CreateMessage(ctx, *upstreamAny.(*anthropic.MessageRequest))
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	// The prior tool turn must reach Anthropic as tool_use + tool_result.
	if !strings.Contains(string(upstreamBody), `"tool_use"`) ||
		!strings.Contains(string(upstreamBody), `"tool_result"`) {
		t.Errorf("upstream body lost the tool turn: %s", upstreamBody)
	}

	coreResp, err := provider.ToCoreResponse(ctx, &upstreamResp)
	if err != nil {
		t.Fatalf("ToCoreResponse: %v", err)
	}
	outAny, err := client.FromCoreResponse(ctx, coreResp)
	if err != nil {
		t.Fatalf("FromCoreResponse: %v", err)
	}
	response := outAny.(*chat.ChatResponse)
	calls := response.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].Function.Name != "ls" || calls[0].ID != "toolu_1" {
		t.Fatalf("tool calls = %+v", calls)
	}
	var arguments string
	if err := json.Unmarshal(calls[0].Function.Arguments, &arguments); err != nil {
		t.Fatalf("arguments must be a JSON string, got %s", calls[0].Function.Arguments)
	}
	if !strings.Contains(arguments, `"path"`) {
		t.Errorf("arguments = %q", arguments)
	}
	if response.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", response.Choices[0].FinishReason)
	}
}

func TestChatIngressE2E_Streaming(t *testing.T) {
	ctx := context.Background()
	cfg := e2eMinimalConfig()
	hooks := format.CorePluginHooks{}.WithDefaults()
	reg := newTestRegistry(t, cfg, hooks)

	client, _ := reg.GetClient(configOpenAIChat)
	clientStream, ok := reg.GetClientStream(configOpenAIChat)
	if !ok {
		t.Fatal("chat ingress client stream adapter not found")
	}
	provider, _ := reg.GetProvider(configAnthropic)
	providerStream, _ := reg.GetProviderStream(configAnthropic)

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(w, "message_start", `{"type":"message_start","message":{"id":"msg_str_002","type":"message","role":"assistant","content":[],"model":"claude-3.5-sonnet-20241022","usage":{"input_tokens":5,"output_tokens":0}}}`)
		sseFlush(w)
		writeSSE(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		sseFlush(w)
		writeSSE(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello from "}}`)
		sseFlush(w)
		writeSSE(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"streaming mock!"}}`)
		sseFlush(w)
		writeSSE(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		sseFlush(w)
		writeSSE(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,"output_tokens":3}}`)
		sseFlush(w)
		writeSSE(w, "message_stop", `{"type":"message_stop"}`)
		sseFlush(w)
	}))
	defer mockSrv.Close()

	chatReq := decodeChatRequest(t, `{
		"model": "claude-3.5-sonnet",
		"max_tokens": 128,
		"stream": true,
		"stream_options": {"include_usage": true},
		"messages": [{"role": "user", "content": "Hello streaming"}]
	}`)

	coreReq, err := client.ToCoreRequest(ctx, chatReq)
	if err != nil {
		t.Fatalf("ToCoreRequest: %v", err)
	}
	if !coreReq.Stream {
		t.Error("expected Stream=true in CoreRequest")
	}

	upstreamAny, err := provider.FromCoreRequest(ctx, coreReq)
	if err != nil {
		t.Fatalf("FromCoreRequest: %v", err)
	}
	anthClient := anthropic.NewClient(anthropic.ClientConfig{
		BaseURL: mockSrv.URL,
		APIKey:  "test-key",
		Client:  mockSrv.Client(),
	})
	stream, err := anthClient.StreamMessage(ctx, *upstreamAny.(*anthropic.MessageRequest))
	if err != nil {
		t.Fatalf("StreamMessage: %v", err)
	}
	defer stream.Close()

	coreEvents, err := providerStream.ToCoreStream(ctx, stream)
	if err != nil {
		t.Fatalf("ToCoreStream: %v", err)
	}

	streamOutAny, err := clientStream.FromCoreStream(ctx, coreReq, coreEvents.Events)
	if err != nil {
		t.Fatalf("FromCoreStream: %v", err)
	}
	frames, ok := streamOutAny.(format.ClientStreamFrames)
	if !ok {
		t.Fatalf("stream result type = %T, want format.ClientStreamFrames", streamOutAny)
	}

	var text strings.Builder
	var finish string
	var usage *chat.Usage
	var sawDone bool
	var roleFirst bool
	first := true
	for frame := range frames.Frames() {
		if frame.Raw == "[DONE]" {
			sawDone = true
			continue
		}
		if sawDone {
			t.Error("frame emitted after the [DONE] sentinel")
		}
		chunk, ok := frame.Data.(chatingress.StreamChunk)
		if !ok {
			continue
		}
		if first {
			roleFirst = len(chunk.Choices) == 1 && chunk.Choices[0].Delta.Role == "assistant"
			first = false
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

	if !roleFirst {
		t.Error("first chunk must announce delta.role = assistant")
	}
	if text.String() != "Hello from streaming mock!" {
		t.Errorf("streamed text = %q", text.String())
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop", finish)
	}
	if usage == nil || usage.PromptTokens != 5 || usage.CompletionTokens != 3 {
		t.Errorf("usage chunk = %+v", usage)
	}
	if !sawDone {
		t.Error("stream did not terminate with [DONE]")
	}
}
