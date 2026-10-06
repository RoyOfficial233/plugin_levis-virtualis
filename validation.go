package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Accept canonical positive decimal identifiers, never normalize caller input.
func validID(id string) bool {
	if id == "" || id[0] < '1' || id[0] > '9' {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n > 0
}

func safeAPIPath(path string) bool {
	u, err := url.Parse(path)
	if err != nil || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || u.IsAbs() || u.Host != "" || u.RawPath != "" || u.Fragment != "" || strings.ContainsAny(u.Path, "\\%") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	if len(parts) > 1 && parts[0] == "instances" && parts[1] != "batch" && !validID(parts[1]) {
		return false
	}
	if len(parts) > 1 && (parts[0] == "firewall" || parts[0] == "trash" || parts[0] == "ip-pools") && !validID(parts[1]) {
		return false
	}
	if len(parts) > 2 && parts[0] == "vpcs" {
		return false
	}
	if len(parts) > 1 && parts[0] == "vpcs" && !validID(parts[1]) {
		return false
	}
	if len(parts) == 4 && parts[0] == "instances" && !validID(parts[3]) {
		return false
	}
	if len(parts) == 5 && parts[0] == "instances" && !validID(parts[3]) {
		return false
	}
	if u.RawQuery != "" {
		allowed := map[string][]string{"/images": {"driver"}, "/vpcs": {"agent_id"}, "/trash": {"page", "page_size"}}
		keys, ok := allowed[u.Path]
		if !ok {
			return false
		}
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return false
		}
		for key, values := range query {
			found := false
			for _, permitted := range keys {
				if key == permitted {
					found = true
				}
			}
			if !found || len(values) != 1 {
				return false
			}
		}
	}
	return true
}

type upstreamError struct {
	HTTPStatus int
	Code       string
	Message    string
}

func (e *upstreamError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("上游 HTTP %d (%s): %s", e.HTTPStatus, e.Code, e.Message)
	}
	return fmt.Sprintf("上游返回 HTTP %d（端点可能不受当前主控支持）", e.HTTPStatus)
}

func parseUpstreamError(resp *http.Response, raw []byte, creds *credentials) error {
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	// Do not expose raw bodies (HTML, credentials, archives or stack traces).
	_ = json.Unmarshal(raw, &body)
	code := strings.ReplaceAll(body.Code, creds.apiKey, "[redacted]")
	code = strings.ReplaceAll(code, creds.apiURL, "[upstream]")
	message := strings.ReplaceAll(body.Message, creds.apiKey, "[redacted]")
	message = strings.ReplaceAll(message, creds.apiURL, "[upstream]")
	return &upstreamError{HTTPStatus: resp.StatusCode, Code: truncate(code, 64), Message: truncate(message, 512)}
}

func resourceNotFound(err error) bool {
	var remote *upstreamError
	return errors.As(err, &remote) && remote.HTTPStatus == http.StatusNotFound && remote.Code == "NOT_FOUND" &&
		remote.Message != "not found"
}

// structuredRouteMissing reports the generic JSON 404 "not found" emitted by
// gin when the route itself is absent on this upstream version.
func structuredRouteMissing(err error) bool {
	var remote *upstreamError
	return errors.As(err, &remote) && remote.HTTPStatus == http.StatusNotFound && remote.Message == "not found"
}

func unsupportedRoute(err error) bool {
	var remote *upstreamError
	return errors.As(err, &remote) && (remote.HTTPStatus == http.StatusNotFound || remote.HTTPStatus == http.StatusMethodNotAllowed) && remote.Code == ""
}
