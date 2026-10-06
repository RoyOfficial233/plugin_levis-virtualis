package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testConfig(server *httptest.Server, key string) map[string]string {
	return map[string]string{"api_url": server.URL, "api_key": key}
}

func TestCredentialsRejectUnsafeURLAndHeaders(t *testing.T) {
	for _, base := range []string{"file:///etc/passwd", "ftp://example.com", "https://user:pass@example.com", "https://example.com?next=x", "https://example.com#secret", "https://example.com/api/../admin", "https://example.com/%2e%2e", "https://example.com/api%2fv1", "https://example.com//admin", "example.com", "https://"} {
		t.Run(base, func(t *testing.T) {
			if _, err := credsFromConfig(map[string]string{"api_url": base, "api_key": "valid-key"}); err == nil {
				t.Fatalf("accepted unsafe api_url %q", base)
			}
		})
	}
	for _, key := range []string{"", " key", "key ", "key\r\nInjected: value", "key\x00"} {
		if _, err := credsFromConfig(map[string]string{"api_url": "https://example.com", "api_key": key}); err == nil {
			t.Fatalf("accepted invalid header value %q", key)
		}
	}
}

func TestAPIRequestDoesNotFollowRedirectWithCredentials(t *testing.T) {
	var received bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = true
		fmt.Fprint(w, `{}`)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/leak", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	p := &virtualisPlugin{client: source.Client()}
	creds, _ := credsFromConfig(testConfig(source, "sensitive-key"))
	if err := p.apiGet(context.Background(), creds, "/images", nil); err == nil {
		t.Fatal("redirect should be rejected")
	}
	if received {
		t.Fatal("redirect target received an authenticated request")
	}
}

func TestAPIResponseRejectsOversizeEvenWithoutOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat(" ", (4<<20)+1))
	}))
	defer srv.Close()
	p := &virtualisPlugin{client: srv.Client()}
	creds, _ := credsFromConfig(testConfig(srv, "test-key"))
	if err := p.apiGet(context.Background(), creds, "/images", nil); err == nil {
		t.Fatal("silently accepted a truncated upstream response")
	}
}
