package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestFetchRouteKeepsContextAliveForSlowStreamingBody(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		response.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(response, "data: first\n\n")
		if flusher, ok := response.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(response, "data: second\n\ndata: [DONE]\n\n")
	}))
	defer upstreamServer.Close()

	upstreamURL, err := url.Parse(upstreamServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	gateway := &AgnesGateway{
		upstream: upstreamURL,
		direct:   upstreamServer.Client(),
		clash:    upstreamServer.Client(),
		routes:   newAgnesRouteSelector(5 * time.Minute),
	}
	incoming := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	result := gateway.fetchRoute(context.Background(), "direct", incoming, []byte(`{"stream":true}`), "test-upstream-key")
	if result.response == nil {
		t.Fatalf("fetchRoute failed before returning headers: %v", result.err)
	}
	stream, readErr := io.ReadAll(result.response.Body)
	closeErr := result.response.Body.Close()
	if readErr != nil {
		t.Fatalf("slow response stream was canceled after headers: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close response body: %v", closeErr)
	}
	text := string(stream)
	if !strings.Contains(text, "data: first") || !strings.Contains(text, "data: second") || !strings.Contains(text, "data: [DONE]") {
		t.Fatalf("incomplete slow stream: %q", text)
	}
}
