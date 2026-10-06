package main

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	requestTimeout  = 90 * time.Second
	recoveryTimeout = 2 * time.Hour
	maxJSONResponse = 4 << 20
)

func safeBasePath(base *url.URL) bool {
	if base.RawPath != "" || strings.ContainsAny(base.Path, "\\%") || strings.Contains(base.Path, "//") {
		return false
	}
	for _, part := range strings.Split(base.Path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// Copy the client instead of mutating shared configuration. Redirects are never
// followed: X-Virtualis-Api-Key is not a standard credential header and Go would
// otherwise forward it to another origin. No credential is retained on p.
func (p *virtualisPlugin) requestClient(timeout time.Duration) *http.Client {
	p.mu.Lock()
	base := p.client
	p.mu.Unlock()
	client := http.Client{Timeout: timeout}
	if base != nil {
		client = *base
		client.Timeout = timeout
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// An injected cookie jar must not mix independent interface credentials.
	client.Jar = nil
	return &client
}
