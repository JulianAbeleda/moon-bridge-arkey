package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"

	"moonbridge/internal/format"
	"moonbridge/internal/protocol/openai"
)

// inboundRequest describes a decoded client request independently of the
// inbound protocol that carried it.
//
// The adapter dispatch path needs only the model, the streaming flag and the
// decoded DTO; everything protocol-specific is handled by the ClientAdapter
// registered under Protocol.
type inboundRequest struct {
	// Protocol is the inbound protocol identifier, matching the key the
	// ClientAdapter is registered under (config.ProtocolOpenAIResponse or
	// config.ProtocolOpenAIChat).
	Protocol string

	// Model is the requested model alias.
	Model string

	// Stream reports whether the client asked for a streaming response.
	Stream bool

	// Raw is the decoded protocol DTO handed to ClientAdapter.ToCoreRequest.
	Raw any
}

// inboundFromResponses builds an inboundRequest for the OpenAI Responses ingress.
func inboundFromResponses(req *openai.ResponsesRequest, protocol string) inboundRequest {
	return inboundRequest{
		Protocol: protocol,
		Model:    req.Model,
		Stream:   req.Stream,
		Raw:      req,
	}
}

// clientStreamFrames normalizes the value returned by
// ClientStreamAdapter.FromCoreStream into protocol-agnostic SSE frames.
//
// Adapters may return either a format.ClientStreamFrames implementation or,
// for the OpenAI Responses adapter that predates it, a typed StreamEvent
// channel.
func clientStreamFrames(result any) (<-chan format.ClientStreamFrame, func() []any, bool) {
	switch typed := result.(type) {
	case format.ClientStreamFrames:
		return typed.Frames(), typed.Buffer, true
	case *openai.OpenAIStreamResult:
		return framesFromOpenAIEvents(typed.Chan()), typed.Buffer, true
	case <-chan openai.StreamEvent:
		return framesFromOpenAIEvents(typed), nil, true
	default:
		return nil, nil, false
	}
}

// framesFromOpenAIEvents adapts a channel of OpenAI Responses stream events to
// the protocol-agnostic frame channel.
func framesFromOpenAIEvents(events <-chan openai.StreamEvent) <-chan format.ClientStreamFrame {
	frames := make(chan format.ClientStreamFrame)
	go func() {
		defer close(frames)
		for event := range events {
			frames <- format.ClientStreamFrame{Event: event.Event, Data: event.Data}
		}
	}()
	return frames
}

// writeSSEFrame writes one server-sent event.
//
// A frame with no Event name is written as a bare data line, which is what the
// Chat Completions wire format expects; Raw is emitted verbatim so adapters can
// send the literal [DONE] sentinel.
func writeSSEFrame(writer http.ResponseWriter, frame format.ClientStreamFrame) error {
	payload := frame.Raw
	if payload == "" {
		payload = "{}"
		if frame.Data != nil {
			encoded, err := json.Marshal(frame.Data)
			if err != nil {
				// An unserializable payload is a bug in one event, not a reason
				// to abandon the stream: emit an empty frame and keep going.
				slog.Default().Warn("SSE frame could not be encoded", "event", frame.Event, "error", err)
			} else {
				payload = string(encoded)
			}
		}
	}
	if frame.Event != "" {
		if _, err := writer.Write([]byte("event: " + frame.Event + "\n")); err != nil {
			return err
		}
	}
	if _, err := writer.Write([]byte("data: " + payload + "\n\n")); err != nil {
		return err
	}
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// teeCoreUsage forwards Core stream events while capturing the terminal usage,
// so the dispatcher can record statistics without knowing the inbound
// protocol's stream representation. The captured value must only be read after
// the downstream frame channel has drained.
func teeCoreUsage(events <-chan format.CoreStreamEvent, usage *format.CoreUsage) <-chan format.CoreStreamEvent {
	out := make(chan format.CoreStreamEvent)
	go func() {
		defer close(out)
		for event := range events {
			if event.Usage != nil {
				*usage = *event.Usage
			}
			out <- event
		}
	}()
	return out
}

// drainClientStream consumes the remaining frames after the client connection
// has failed. Without it the adapter's producer goroutine — and the upstream
// stream it reads from — block on an unread channel for the process lifetime.
func drainClientStream(frames <-chan format.ClientStreamFrame) {
	go func() {
		for range frames {
		}
	}()
}

// isNilResponse reports whether a client adapter produced no usable response.
// A typed nil pointer is not == nil, so the interface comparison alone would
// let a `null` body through with a 200.
func isNilResponse(response any) bool {
	if response == nil {
		return true
	}
	value := reflect.ValueOf(response)
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func:
		return value.IsNil()
	default:
		return false
	}
}
