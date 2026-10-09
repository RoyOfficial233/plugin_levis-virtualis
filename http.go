package main

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

const (
	requestTimeout  = 90 * time.Second
	recoveryTimeout = 2 * time.Hour
	maxJSONResponse = 4 << 20
)

// manageTimeout picks the upstream HTTP budget for a ManageHost action.
// Mutations that can replace disks or permanently delete data may legitimately
// run for hours on a loaded agent; every other action keeps the short budget
// so a hung request fails fast instead of pinning the host RPC for 2 hours.
func manageTimeout(action pb.HostAction) time.Duration {
	switch action {
	case pb.HostAction_HOST_ACTION_RESIZE, pb.HostAction_HOST_ACTION_TERMINATE, pb.HostAction_HOST_ACTION_REINSTALL:
		return recoveryTimeout
	default:
		return requestTimeout
	}
}

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
