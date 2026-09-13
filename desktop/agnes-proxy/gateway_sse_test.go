package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGatewayRetriesEmbeddedSSEErrorBeforeVisibleContent(t *testing.T) {
	logger, err := NewJSONLLogger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := NewAgnesKeyMonitor(logger, time.Second)
	keys.reader = func() (string, string) { return "agnes-sse-retry-secret", "test" }
	keys.Refresh()
	gateway, err := NewAgnesGateway(0, keys, nil, logger, "local-sse-retry-token", nil)
	if err != nil {
		t.Fatal(err)
	}

	attempts := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		body := `data: {"id":"metadata-only","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
			`data: {"error":{"message":"litellm.APIConnectionError: connection closed","code":500}}` + "\n\n"
		if attempts > 1 {
			body = `data: {"choices":[{"delta":{"content":"recovered"}}]}` + "\n\n" + "data: [DONE]\n\n"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})
	gateway.direct = &http.Client{Transport: transport}
	gateway.clash = &http.Client{Transport: transport}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"agnes-3.0-flash","stream":true,"messages":[]}`))
	request.Header.Set("Authorization", "Bearer local-sse-retry-token")
	response := httptest.NewRecorder()
	started := time.Now()
	gateway.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "recovered") {
		t.Fatalf("unexpected response: status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(strings.ToLower(response.Body.String()), "connection closed") {
		t.Fatal("embedded upstream error leaked to the client")
	}
	if attempts != 2 || time.Since(started) < 1900*time.Millisecond {
		t.Fatalf("embedded SSE error was not retried with backoff: attempts=%d duration=%s", attempts, time.Since(started))
	}
	contents := readLogs(t, logger.Dir())
	if !strings.Contains(contents, "response_stream_retry_scheduled") || !strings.Contains(contents, "request_succeeded") || strings.Contains(contents, keys.APIKey()) {
		t.Fatal("embedded SSE retry was not logged safely")
	}
}

func TestForwardAgnesEventStreamRequiresDoneMarker(t *testing.T) {
	response := httptest.NewRecorder()
	body := strings.NewReader(`data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n")
	bytesForwarded, err := forwardAgnesStream(response, http.StatusOK, body, "text/event-stream; charset=utf-8")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected unexpected EOF, got %v", err)
	}
	if bytesForwarded == 0 || !strings.Contains(response.Body.String(), "partial") {
		t.Fatalf("partial content was not accounted for: bytes=%d body=%s", bytesForwarded, response.Body.String())
	}
}
