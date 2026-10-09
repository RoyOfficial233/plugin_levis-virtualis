package main

// PLG-F07: redaction must survive JSON escaping. Secrets are replaced on the
// decoded string level and the document re-encoded, so a key or URL
// containing quotes, backslashes or Go HTML-escaped characters cannot be
// echoed back verbatim after the browser decodes the JSON.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

func redactionFixtures() map[string]*credentials {
	return map[string]*credentials{
		"plain": {apiURL: "http://upstream.example", apiKey: "plain-key"},
		"quotes": {apiURL: "http://upstream.example", apiKey: `q"uo"te-key`},
		"backslash": {apiURL: `http://upstream.example`, apiKey: `back\slash-key`},
		"html_url": {apiURL: "http://upstream.example/a&b<c>d", apiKey: "html-key"},
		"unicode_escape": {apiURL: "http://upstream.example", apiKey: "uni€key"},
		"combined": {apiURL: "http://upstream.example/pa&th", apiKey: `x"y\z&<>&key`},
	}
}

func TestRedactSecretsCoversEscapedCharacters(t *testing.T) {
	for name, creds := range redactionFixtures() {
		t.Run(name, func(t *testing.T) {
			// The allowlisted free-text field remark echoes both secrets.
			data := []byte(fmt.Sprintf(`{"id":4,"remark":"leak %s and %s","status":"available"}`, creds.apiKey, creds.apiURL))
			out := redactSecrets(data, creds)
			if !json.Valid(out) {
				t.Fatalf("redacted output is not valid JSON: %s", out)
			}
			var decoded map[string]any
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatalf("redacted output does not decode: %v", err)
			}
			remark, _ := decoded["remark"].(string)
			if strings.Contains(remark, creds.apiKey) || strings.Contains(remark, creds.apiURL) {
				t.Fatalf("secret survived redaction: %q (key=%q url=%q)", remark, creds.apiKey, creds.apiURL)
			}
			if !strings.Contains(remark, "[redacted]") {
				t.Fatalf("redaction marker missing: %q", remark)
			}
			// Raw encoded bytes must not carry the secret either.
			if strings.Contains(string(out), creds.apiKey) || strings.Contains(string(out), creds.apiURL) {
				t.Fatalf("secret present in encoded output: %s", out)
			}
		})
	}
}

// A secret embedded inside the DNS string array must be redacted as well.
func TestRedactSecretsCoversArrayValues(t *testing.T) {
	creds := &credentials{apiURL: "http://upstream.example", apiKey: `arr"ay-key`}
	data := []byte(fmt.Sprintf(`{"dns":["%s","1.1.1.1"]}`, creds.apiKey))
	out := redactSecrets(data, creds)
	if !json.Valid(out) {
		t.Fatalf("invalid JSON: %s", out)
	}
	var decoded struct {
		DNS []string `json:"dns"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, entry := range decoded.DNS {
		if strings.Contains(entry, creds.apiKey) {
			t.Fatalf("array secret survived: %q", entry)
		}
	}
}

func TestRedactSecretsPreservesNonSensitiveValues(t *testing.T) {
	creds := &credentials{apiURL: "http://upstream.example", apiKey: "keep-key"}
	data := []byte(`{"id":4,"name":"backup","status":"available","size_bytes":1024}`)
	out := redactSecrets(data, creds)
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["name"] != "backup" || decoded["status"] != "available" || decoded["size_bytes"] != float64(1024) {
		t.Fatalf("non-sensitive values lost: %s", out)
	}
}

// End-to-end: the sanitized HostOperation payload must never leak either
// credential through the encoding boundary. Keys may contain any printable
// ASCII (credsFromConfig allows quotes/backslashes); URLs may carry & < >
// path characters, which Go's JSON encoder HTML-escapes.
func TestHostOperationRedactionSurvivesJSONEncoding(t *testing.T) {
	for name, tc := range map[string]struct{ key, urlSuffix string }{
		"plain":         {"plain-key", ""},
		"quotes_key":    {`q"uo"te-key`, ""},
		"backslash_key": {`back\slash-key`, ""},
		// & in a URL path is legal (no RawPath) but HTML-escaped by Go's
		// JSON encoder; < > cannot appear in a validated api_url path.
		"html_url": {"html-key", "/pa&th&more"},
		"combined": {`x"y\z&<>&key`, "/pa&th"},
	} {
		t.Run(name, func(t *testing.T) {
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				apiURL := srvURL(t, srv, tc.urlSuffix)
				remark := "echo " + tc.key + " " + apiURL
				encoded, err := json.Marshal(map[string]any{"items": []any{map[string]any{"id": 4, "instance_id": 7, "name": "b", "remark": remark, "status": "available"}}})
				if err != nil {
					t.Fatal(err)
				}
				w.Write(encoded)
			}))
			defer srv.Close()
			apiURL := srvURL(t, srv, tc.urlSuffix)
			reply, err := (&virtualisPlugin{client: srv.Client()}).HostOperation(t.Context(), &pb.HostOperationRequest{
				HostId: "7", Action: "backup_list",
				InterfaceConfig: map[string]string{"api_url": apiURL, "api_key": tc.key},
			})
			if err != nil || reply.GetError() != "" {
				t.Fatalf("reply=%v err=%v", reply, err)
			}
			raw := reply.GetDataJson()
			if !json.Valid([]byte(raw)) {
				t.Fatalf("data_json invalid: %s", raw)
			}
			var decoded map[string]any
			if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
				t.Fatal(err)
			}
			items, _ := decoded["items"].([]any)
			if len(items) != 1 {
				t.Fatalf("items lost: %s", raw)
			}
			item, _ := items[0].(map[string]any)
			remark, _ := item["remark"].(string)
			if strings.Contains(remark, tc.key) || strings.Contains(remark, apiURL) {
				t.Fatalf("secret decoded back after redaction: %q", remark)
			}
		})
	}
}

func srvURL(t *testing.T, srv *httptest.Server, suffix string) string {
	t.Helper()
	if srv == nil {
		return ""
	}
	return srv.URL + suffix
}
