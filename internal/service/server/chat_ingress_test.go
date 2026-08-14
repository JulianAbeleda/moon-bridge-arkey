package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"moonbridge/internal/service/server"
)

func TestChatCompletionsRouteRejectsNonPost(t *testing.T) {
	handler := server.New(server.Config{})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestChatCompletionsRouteRejectsInvalidJSON(t *testing.T) {
	handler := server.New(server.Config{})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(
		http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":`)))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	assertChatErrorCode(t, recorder.Body.Bytes(), "invalid_json")
}

func TestChatCompletionsRouteRequiresModel(t *testing.T) {
	handler := server.New(server.Config{})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(
		http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	assertChatErrorCode(t, recorder.Body.Bytes(), "invalid_model")
}

func TestChatCompletionsRouteReportsUnknownModel(t *testing.T) {
	handler := server.New(server.Config{})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(
		http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"nope","messages":[{"role":"user","content":"hi"}]}`)))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	assertChatErrorCode(t, recorder.Body.Bytes(), "model_not_found")
}

// assertChatErrorCode checks the error envelope a Chat Completions client parses.
func assertChatErrorCode(t *testing.T, body []byte, wantCode string) {
	t.Helper()
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode error envelope: %v (body = %s)", err, body)
	}
	if payload.Error.Code != wantCode {
		t.Fatalf("error.code = %q, want %q (body = %s)", payload.Error.Code, wantCode, body)
	}
	if payload.Error.Message == "" {
		t.Errorf("error.message is empty (body = %s)", body)
	}
}
