// Package format defines protocol-agnostic Core types for MoonBridge.
//
// This file defines the protocol-agnostic SSE frame produced by inbound
// (client) stream adapters. It lets the dispatcher write a streaming response
// without knowing which inbound protocol produced it: OpenAI Responses emits
// named events, OpenAI Chat Completions emits anonymous data-only frames
// terminated by a literal [DONE] payload.
package format

// ClientStreamFrame is a single server-sent event written back to the client.
//
// Exactly one of Data or Raw carries the payload:
//   - Data is JSON-marshalled into the "data:" line.
//   - Raw, when non-empty, is written verbatim as the "data:" line and takes
//     precedence over Data (used for the Chat Completions "[DONE]" sentinel).
//
// Event is optional. When empty no "event:" line is written, which is what
// the Chat Completions wire format expects.
type ClientStreamFrame struct {
	Event string
	Data  any
	Raw   string
}

// ClientStreamFrames is implemented by the value returned from
// ClientStreamAdapter.FromCoreStream when the adapter emits protocol-agnostic
// frames. Adapters that predate this interface return their own typed channel
// and are normalized by the dispatcher instead.
type ClientStreamFrames interface {
	// Frames returns the channel of frames to write. It is closed when the
	// stream is exhausted.
	Frames() <-chan ClientStreamFrame

	// Buffer returns the captured frames for trace and plugin post-processing.
	// It must only be called after Frames is fully consumed.
	Buffer() []any
}
