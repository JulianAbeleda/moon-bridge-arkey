package chatingress

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"moonbridge/internal/format"
	"moonbridge/internal/protocol/chat"
)

// StreamResult carries the Chat Completions SSE frames produced from a Core
// stream. It implements format.ClientStreamFrames.
type StreamResult struct {
	frames <-chan format.ClientStreamFrame
	buffer func() []any
}

// Frames returns the channel of SSE frames to write to the client.
func (r *StreamResult) Frames() <-chan format.ClientStreamFrame { return r.frames }

// Buffer returns the captured chunks for trace and plugin post-processing.
func (r *StreamResult) Buffer() []any {
	if r.buffer == nil {
		return nil
	}
	return r.buffer()
}

// FromCoreStream converts a Core stream into Chat Completions SSE frames.
//
// Wire shape (OpenAI Chat Completions):
//   - every frame is an anonymous `data:` line holding one StreamChunk
//   - the first chunk carries delta.role = "assistant"
//   - text arrives as delta.content, reasoning as delta.reasoning_content
//   - tool calls arrive as delta.tool_calls entries: the opening chunk carries
//     index/id/type/name, later chunks carry index plus an argument fragment
//   - the final content chunk carries finish_reason
//   - a usage-only chunk follows when the client set stream_options.include_usage
//   - the stream terminates with the literal `data: [DONE]`
func (a *Adapter) FromCoreStream(ctx context.Context, req *format.CoreRequest, events <-chan format.CoreStreamEvent) (any, error) {
	frames := make(chan format.ClientStreamFrame)
	bufferReady := make(chan struct{})

	var buffer []any
	var bufferMutex sync.Mutex

	go func() {
		defer close(bufferReady)
		a.streamLoop(ctx, req, events, frames, &buffer, &bufferMutex)
	}()

	return &StreamResult{
		frames: frames,
		buffer: func() []any {
			<-bufferReady
			bufferMutex.Lock()
			defer bufferMutex.Unlock()
			captured := make([]any, len(buffer))
			copy(captured, buffer)
			return captured
		},
	}, nil
}

// streamState tracks what the client has already been told about one Core
// content block.
type streamState struct {
	kind      string // "text" | "reasoning" | "tool_use"
	toolIndex int
}

func (a *Adapter) streamLoop(
	ctx context.Context,
	coreReq *format.CoreRequest,
	events <-chan format.CoreStreamEvent,
	frames chan<- format.ClientStreamFrame,
	buffer *[]any,
	bufferMutex *sync.Mutex,
) {
	defer close(frames)

	model := ""
	includeUsage := false
	if coreReq != nil {
		model = coreReq.Model
		if extension, ok := coreReq.Extensions["openai_chat"].(map[string]any); ok {
			includeUsage, _ = extension["include_usage"].(bool)
		}
	}

	id := "chatcmpl-moonbridge"
	created := a.now().Unix()

	send := func(frame format.ClientStreamFrame) {
		bufferMutex.Lock()
		if len(*buffer) < 1024 && frame.Data != nil {
			*buffer = append(*buffer, frame.Data)
		}
		bufferMutex.Unlock()
		frames <- frame
	}

	sendChunk := func(delta StreamDelta, finish *string) {
		send(format.ClientStreamFrame{Data: StreamChunk{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []StreamChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		}})
	}

	blocks := make(map[int]*streamState)
	nextToolIndex := 0
	roleSent := false
	var finishedReason string
	var finalUsage *format.CoreUsage
	var text strings.Builder

	openRole := func() {
		if roleSent {
			return
		}
		roleSent = true
		sendChunk(StreamDelta{Role: "assistant"}, nil)
	}

	for event := range events {
		if a.hooks.OnStreamEvent(ctx, event) {
			continue
		}

		switch event.Type {
		case format.CoreEventCreated:
			if event.ItemID != "" {
				id = completionID(event.ItemID)
			}
			if event.Model != "" {
				model = event.Model
			}
			openRole()

		case format.CoreEventInProgress, format.CorePing:
			// No Chat Completions equivalent.

		case format.CoreContentBlockStarted:
			if event.ContentBlock == nil {
				continue
			}
			openRole()
			switch event.ContentBlock.Type {
			case "tool_use":
				state := &streamState{kind: "tool_use", toolIndex: nextToolIndex}
				nextToolIndex++
				blocks[event.Index] = state
				callID := event.ContentBlock.ToolUseID
				if callID == "" {
					callID = fmt.Sprintf("call_%d", state.toolIndex)
				}
				sendChunk(StreamDelta{ToolCalls: []StreamToolCall{{
					Index:    state.toolIndex,
					ID:       callID,
					Type:     "function",
					Function: &StreamToolCallFunc{Name: event.ContentBlock.ToolName, Arguments: ""},
				}}}, nil)
			case "reasoning":
				blocks[event.Index] = &streamState{kind: "reasoning"}
			default:
				blocks[event.Index] = &streamState{kind: "text"}
			}

		case format.CoreTextDelta:
			if event.Delta == "" {
				continue
			}
			openRole()
			if state, known := blocks[event.Index]; known && state.kind == "reasoning" {
				sendChunk(StreamDelta{ReasoningContent: event.Delta}, nil)
				continue
			}
			text.WriteString(event.Delta)
			sendChunk(StreamDelta{Content: event.Delta}, nil)

		case format.CoreToolCallArgsDelta:
			if event.Delta == "" {
				continue
			}
			openRole()
			state, known := blocks[event.Index]
			if !known {
				state = &streamState{kind: "tool_use", toolIndex: nextToolIndex}
				nextToolIndex++
				blocks[event.Index] = state
			}
			sendChunk(StreamDelta{ToolCalls: []StreamToolCall{{
				Index:    state.toolIndex,
				Function: &StreamToolCallFunc{Arguments: event.Delta},
			}}}, nil)

		case format.CoreContentBlockDone, format.CoreTextDone, format.CoreToolCallArgsDone, format.CoreItemAdded, format.CoreItemDone:
			// Chat Completions has no per-block terminator; finish_reason on the
			// last chunk carries the same information.

		case format.CoreEventCompleted, format.CoreEventIncomplete:
			openRole()
			if event.Usage != nil {
				finalUsage = event.Usage
			}
			finishedReason = finishReason(event.StopReason, nextToolIndex > 0)
			if event.Type == format.CoreEventIncomplete && event.StopReason == "" {
				finishedReason = "length"
			}

		case format.CoreEventFailed:
			openRole()
			message := "upstream stream failed"
			errorType := "server_error"
			if event.Error != nil {
				if event.Error.Message != "" {
					message = event.Error.Message
				}
				if event.Error.Type != "" {
					errorType = event.Error.Type
				}
			}
			send(format.ClientStreamFrame{Data: ErrorResponse{Error: ErrorObject{Message: message, Type: errorType}}})
			finishedReason = "stop"
		}
	}

	openRole()
	if finishedReason == "" {
		finishedReason = finishReason("", nextToolIndex > 0)
	}
	sendChunk(StreamDelta{}, &finishedReason)

	if includeUsage {
		usageChunk := StreamChunk{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []StreamChoice{},
			Usage:   &chat.Usage{},
		}
		if finalUsage != nil {
			usageChunk.Usage = &chat.Usage{
				PromptTokens:     finalUsage.InputTokens,
				CompletionTokens: finalUsage.OutputTokens,
				TotalTokens:      totalTokens(*finalUsage),
			}
			if finalUsage.CachedInputTokens > 0 {
				usageChunk.Usage.PromptTokensDetails = &chat.PromptTokensDetails{CachedTokens: finalUsage.CachedInputTokens}
			}
		}
		send(format.ClientStreamFrame{Data: usageChunk})
	}

	a.hooks.OnStreamComplete(ctx, model, text.String())
	send(format.ClientStreamFrame{Raw: "[DONE]"})
}
