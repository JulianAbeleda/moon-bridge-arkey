package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"moonbridge/internal/config"
	"moonbridge/internal/protocol/chatingress"
	"moonbridge/internal/protocol/openai"
	mbtrace "moonbridge/internal/service/trace"
)

// handleChatCompletions serves the inbound OpenAI Chat Completions ingress
// (POST /v1/chat/completions).
//
// It is the Chat-protocol twin of handleResponses: decode, resolve the route,
// then hand off to the shared adapter dispatch path, which converts through
// Core to whichever upstream protocol the resolved provider speaks.
//
// Clients that speak Chat Completions rather than Responses — Crush, Cline,
// Aider, the OpenAI SDKs' completions surface — reach every configured
// provider through this route.
func (server *Server) handleChatCompletions(writer http.ResponseWriter, request *http.Request) {
	log := slog.Default().With("path", request.URL.Path, "method", request.Method, "remote", request.RemoteAddr)
	log.Debug("收到 chat/completions 请求")
	requestStart := time.Now()

	if request.Method != http.MethodPost {
		writeChatError(writer, http.StatusMethodNotAllowed, "方法不允许", "invalid_request_error", "method_not_allowed")
		return
	}

	server.sessionForRequest(request)

	body, err := io.ReadAll(request.Body)
	record := mbtrace.Record{HTTPRequest: mbtrace.NewHTTPRequest(request), OpenAIRequest: mbtrace.RawJSONOrString(body)}
	if err != nil {
		log.Error("读取请求体失败", "error", err)
		record.Error = traceError("read_chat_request", err)
		server.writeTrace(record)
		writeChatError(writer, http.StatusBadRequest, "读取请求体失败", "invalid_request_error", "invalid_request_body")
		return
	}

	var chatRequest chatingress.Request
	if err := json.Unmarshal(body, &chatRequest); err != nil {
		log.Warn("无效的 JSON 请求体", "error", err)
		record.Error = traceError("decode_chat_request", err)
		server.writeTrace(record)
		writeChatError(writer, http.StatusBadRequest, "无效的 JSON 请求体", "invalid_request_error", "invalid_json")
		return
	}
	if chatRequest.Model == "" {
		record.Error = traceError("empty_model", fmt.Errorf("model is required"))
		server.writeTrace(record)
		writeChatError(writer, http.StatusBadRequest, "model 不能为空", "invalid_request_error", "invalid_model")
		return
	}

	record.Model = chatRequest.Model
	resolvedRoute, resolveErr := server.resolveModelOrFallback(chatRequest.Model)
	if resolveErr != nil {
		log.Warn("请求了未知模型", "model", chatRequest.Model)
		record.Error = traceError("model_not_found", fmt.Errorf("model %q not found", chatRequest.Model))
		server.writeTrace(record)
		writeChatError(writer, http.StatusNotFound,
			fmt.Sprintf("unknown model: %q", chatRequest.Model), "invalid_request_error", "model_not_found")
		return
	}

	filteredCandidates, filterReason := server.filterCandidatesByImage(
		resolvedRoute.Candidates, chatRequestHasImage(chatRequest))
	if len(filteredCandidates) == 0 {
		log.Warn("过滤后无可用提供商", "model", chatRequest.Model, "reason", filterReason)
		record.Error = traceError("provider_filtered", fmt.Errorf("candidates filtered: %s", filterReason))
		server.writeTrace(record)
		writeChatError(writer, http.StatusBadGateway,
			fmt.Sprintf("no available provider for model %q with the requested features", chatRequest.Model),
			"invalid_request_error", "provider_error")
		return
	}
	resolvedRoute.Candidates = filteredCandidates
	if filterReason != "" {
		log.Info("候选过滤", "model", chatRequest.Model, "reason", filterReason)
	}

	preferred, ok := resolvedRoute.Preferred()
	if !ok {
		log.Error("模型解析结果无可用提供商", "model", chatRequest.Model)
		record.Error = traceError("provider_error", fmt.Errorf("no available provider for %q", chatRequest.Model))
		server.writeTrace(record)
		writeChatError(writer, http.StatusBadGateway,
			fmt.Sprintf("no available provider for model %q", chatRequest.Model), "server_error", "provider_error")
		return
	}

	inbound := inboundRequest{
		Protocol: config.ProtocolOpenAIChat,
		Model:    chatRequest.Model,
		Stream:   chatRequest.Stream,
		Raw:      &chatRequest,
	}

	// Unlike the Responses ingress, there is no passthrough to fall back on:
	// every upstream is reached through a ProviderAdapter. Anthropic, Google
	// GenAI and OpenAI Chat upstreams have one; OpenAI Responses upstreams do
	// not — /v1/responses serves those by forwarding the body verbatim, which a
	// Chat Completions request cannot do.
	if server.adapterRegistry != nil {
		if _, ok := server.adapterRegistry.GetProvider(preferred.Protocol); ok {
			server.handleWithAdapters(writer, request, inbound, resolvedRoute)
			return
		}
	}

	log.Error("no adapter path configured", "model", chatRequest.Model, "protocol", preferred.Protocol)
	record.Error = traceError("no_adapter_path", fmt.Errorf("no adapter path for protocol %q", preferred.Protocol))
	server.writeTrace(record)
	writeChatError(writer, http.StatusBadGateway,
		fmt.Sprintf("model %q resolves to a %q upstream, which the /v1/chat/completions ingress cannot reach; route it to an anthropic, google-genai or openai-chat provider, or use /v1/responses",
			chatRequest.Model, preferred.Protocol),
		"invalid_request_error", "unsupported_upstream_protocol")
	server.onRequestCompleted(
		chatRequest.Model, "", "", requestStart,
		zeroUsage(string(preferred.Protocol), "none"), 0, "error", "no adapter path",
	)
}

// chatRequestHasImage reports whether any message carries image content.
func chatRequestHasImage(request chatingress.Request) bool {
	for _, message := range request.Messages {
		parts, ok := message.Content.([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			partMap, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch partMap["type"] {
			case "image_url", "input_image", "image":
				return true
			}
		}
	}
	return false
}

// writeChatError writes a Chat Completions error envelope. The shape is
// identical to the Responses error envelope, so downstream trace and logging
// keep working unchanged.
func writeChatError(writer http.ResponseWriter, status int, message, errorType, code string) {
	writeJSON(writer, status, openai.ErrorResponse{Error: openai.ErrorObject{
		Message: message,
		Type:    errorType,
		Code:    code,
	}})
}
