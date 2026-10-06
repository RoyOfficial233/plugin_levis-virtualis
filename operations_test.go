package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

func TestRecoveryOperationsMapFixedRoutesAndSanitizeResults(t *testing.T) {
	for _, tc := range []struct{ action, payload, method, path, response string }{
		{"snapshot_list", `{}`, "GET", "/instances/7/snapshots", `{"items":[{"id":3,"instance_id":7,"name":"safe","size_bytes":0,"status":"ready","file_path":"C:/secret","api_key":"unsafe"}]}`},
		{"snapshot_create", `{"name":"snap-1","remark":"before change"}`, "POST", "/instances/7/snapshots", `{"id":3,"name":"snap-1","size_bytes":10,"file_path":"secret"}`},
		{"snapshot_restore", `{"snapshot_id":3}`, "POST", "/instances/7/snapshots/3/restore", `{"id":7,"status":"stopped","ssh_password":"secret","agent":{"token":"unsafe"}}`},
		{"snapshot_delete", `{"snapshot_id":3}`, "DELETE", "/instances/7/snapshots/3", ""},
		{"backup_list", `{}`, "GET", "/instances/7/backups", `{"items":[{"id":4,"driver":"qemu","checksum":"abc","file_path":"secret"}]}`},
		{"backup_create", `{"name":"backup-1","remark":"weekly"}`, "POST", "/instances/7/backups", `{"id":4,"driver":"qemu","checksum":"abc","path":"secret"}`},
		{"backup_restore", `{"backup_id":4}`, "POST", "/instances/7/backups/4/restore", `{"id":7,"status":"stopped","config_json":"secret"}`},
		{"backup_delete", `{"backup_id":4}`, "DELETE", "/instances/7/backups/4", ""},
	} {
		t.Run(tc.action, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != tc.method || r.URL.Path != "/api/v1"+tc.path {
					t.Errorf("route = %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("X-Virtualis-Api-Key") != "feature-key" {
					t.Error("credentials missing")
				}
				if tc.method == "POST" && strings.HasSuffix(tc.action, "_create") {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body["name"] == nil || body["remark"] == nil || len(body) != 2 {
						t.Errorf("body = %v", body)
					}
				}
				if tc.response == "" {
					w.WriteHeader(204)
				} else {
					fmt.Fprint(w, tc.response)
				}
			}))
			defer srv.Close()
			p := &virtualisPlugin{client: srv.Client()}
			reply, err := p.HostOperation(context.Background(), &pb.HostOperationRequest{HostId: "7", Action: tc.action, PayloadJson: tc.payload, InterfaceConfig: testConfig(srv, "feature-key")})
			if err != nil || reply.GetError() != "" || !json.Valid([]byte(reply.GetDataJson())) || calls.Load() != 1 {
				t.Fatalf("reply=%v err=%v calls=%d", reply, err, calls.Load())
			}
			for _, secret := range []string{"file_path", "api_key", "ssh_password", "config_json", "secret", "unsafe"} {
				if strings.Contains(reply.GetDataJson(), secret) {
					t.Fatalf("sensitive data leaked: %s", reply.GetDataJson())
				}
			}
		})
	}
}

func TestHostOperationsRejectArbitraryActionsAndPayloads(t *testing.T) {
	for _, tc := range []struct{ action, host, payload string }{
		{"GET", "7", `{"url":"http://evil"}`}, {"snapshot_list/../../admin", "7", `{}`}, {"backup_download", "7", `{"backup_id":4}`},
		{"snapshot_create", "7", `{"name":"ok","path":"/etc/passwd"}`}, {"snapshot_create", "7", `{"name":"../bad"}`},
		{"snapshot_create", "7", `{"name":"ok","name":"shadow"}`}, {"snapshot_list", "7", `null`}, {"snapshot_list", "7", `{} {}`},
		{"snapshot_list", "7/../../trash", `{}`}, {"backup_restore", "7", `{"backup_id":"../4"}`}, {"snapshot_delete", "7", `{"snapshot_id":0}`},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{}`) }))
			defer srv.Close()
			p := &virtualisPlugin{client: srv.Client()}
			reply, err := p.HostOperation(context.Background(), &pb.HostOperationRequest{HostId: tc.host, Action: tc.action, PayloadJson: tc.payload, InterfaceConfig: testConfig(srv, "feature-key")})
			if err == nil && reply.GetError() == "" {
				t.Fatal("unsafe operation accepted")
			}
			if calls.Load() != 0 {
				t.Fatal("unsafe operation reached upstream")
			}
		})
	}
}

func TestHostOperationRequiresCredentialsEveryRequest(t *testing.T) {
	p := &virtualisPlugin{}
	for _, config := range []map[string]string{nil, {"api_url": "http://example.com"}, {"api_key": "some-key"}} {
		if _, err := p.HostOperation(context.Background(), &pb.HostOperationRequest{HostId: "7", Action: "snapshot_list", InterfaceConfig: config}); err == nil {
			t.Fatal("missing credentials were not checked")
		}
	}
}
