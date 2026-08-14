// Package chatingress implements the inbound OpenAI Chat Completions protocol
// for MoonBridge — the client side of the bridge, mirroring internal/protocol/openai
// (which handles inbound OpenAI Responses).
//
// internal/protocol/chat holds the same wire DTOs for the *upstream* direction;
// this package reuses those message/tool DTOs and adds an inbound request type,
// because inbound clients differ from upstream servers in what they send
// (notably `max_tokens`, which the Chat Completions API still accepts and which
// Crush, Cline, Aider and other clients emit instead of `max_completion_tokens`).
package chatingress

import (
	"encoding/json"
	"fmt"

	"moonbridge/internal/protocol/chat"
)

// Request is an inbound POST /v1/chat/completions body.
//
// Unknown fields are ignored rather than rejected: clients routinely send
// provider-specific extras (logit_bias, seed, service_tier, …) that do not
// survive protocol conversion anyway.
type Request struct {
	Model    string             `json:"model"`
	Messages []chat.ChatMessage `json:"messages"`

	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`

	// MaxTokens is the legacy field; MaxCompletionTokens is its replacement.
	// EffectiveMaxTokens resolves the two.
	MaxTokens           int `json:"max_tokens,omitempty"`
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`

	Stop StopSequences `json:"stop,omitempty"`

	Stream        bool                `json:"stream,omitempty"`
	StreamOptions *chat.StreamOptions `json:"stream_options,omitempty"`

	Tools             []chat.ChatTool `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`

	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	User            string         `json:"user,omitempty"`
}

// EffectiveMaxTokens returns the output-token ceiling the client asked for,
// preferring the modern field when both are present.
func (r Request) EffectiveMaxTokens() int {
	if r.MaxCompletionTokens > 0 {
		return r.MaxCompletionTokens
	}
	return r.MaxTokens
}

// IncludeUsage reports whether the client asked for a usage-bearing final chunk.
func (r Request) IncludeUsage() bool {
	return r.StreamOptions != nil && r.StreamOptions.IncludeUsage
}

// StopSequences accepts the Chat Completions `stop` field in both of its
// documented shapes: a bare string or an array of strings.
type StopSequences []string

// UnmarshalJSON decodes either "x" or ["x","y"] into a string slice.
func (s *StopSequences) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*s = nil
		return nil
	}
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		if single == "" {
			*s = nil
			return nil
		}
		*s = StopSequences{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("stop must be a string or an array of strings")
	}
	*s = StopSequences(many)
	return nil
}

// MarshalJSON emits the slice form, which is valid for both shapes.
func (s StopSequences) MarshalJSON() ([]byte, error) {
	return json.Marshal([]string(s))
}

// StreamToolCall is a tool call inside a streaming delta.
//
// It is deliberately not chat.ToolCall: on the wire, only the opening chunk of
// a tool call carries id/type/function.name, and every following chunk carries
// just the index and an argument fragment. chat.ToolCall has no omitempty on
// those fields, so reusing it would emit `"id":"","type":""` on continuation
// chunks — which clients that assign (rather than concatenate) deltas read as
// "the id is now empty".
type StreamToolCall struct {
	Index    int                 `json:"index"`
	ID       string              `json:"id,omitempty"`
	Type     string              `json:"type,omitempty"`
	Function *StreamToolCallFunc `json:"function,omitempty"`
}

// StreamToolCallFunc carries the function name (opening chunk only) and the
// argument fragment for one streaming tool call.
type StreamToolCallFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// StreamDelta is the incremental payload of a streaming choice.
type StreamDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          string           `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []StreamToolCall `json:"tool_calls,omitempty"`
}

// StreamChoice is one choice in a streaming chunk.
type StreamChoice struct {
	Index        int         `json:"index"`
	Delta        StreamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

// StreamChunk is the SSE data payload of a streaming Chat Completions response.
type StreamChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *chat.Usage  `json:"usage,omitempty"`
}

// ErrorResponse is the Chat Completions error envelope.
type ErrorResponse struct {
	Error ErrorObject `json:"error"`
}

// ErrorObject describes a single Chat Completions error.
type ErrorObject struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}
