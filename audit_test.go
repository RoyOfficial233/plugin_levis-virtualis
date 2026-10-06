package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

func TestResizeRetryAlreadyAppliedSucceedsWithoutSecondMutation(t *testing.T) {
	var writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
		}
		fmt.Fprint(w, `{"id":7,"status":"running","spec":{"cpu":2,"memory_mb":2048,"disk_gb":20,"arch":"x86_64"},"network":{"mode":"nat","bandwidth_mbps":100,"traffic_gb":200}}`)
	}))
	defer srv.Close()
	p := &virtualisPlugin{client: srv.Client()}
	reply, err := p.ManageHost(context.Background(), &pb.ManageHostRequest{HostId: "7", Action: pb.HostAction_HOST_ACTION_RESIZE, OperationId: "change-42", Resources: &pb.HostResources{Cpu: 2, MemoryMb: 2048, DiskGb: 20, BandwidthMbps: 100, TrafficGb: 200}, InterfaceConfig: testConfig(srv, "key")})
	if err != nil || !reply.GetSuccess() || writes.Load() != 0 {
		t.Fatalf("already-applied retry: %v %v writes=%d", reply, err, writes.Load())
	}
}

func TestResizeDoesNotAcceptMalformedUpstreamSuccess(t *testing.T) {
	for _, response := range []string{"", `null`, `{"error":"not applied"}`, `{"id":999}`, `<html>proxy login</html>`} {
		t.Run(response, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					fmt.Fprint(w, `{"status":"stopped","spec":{"cpu":1,"memory_mb":512,"disk_gb":10,"arch":"x86_64"},"network":{"mode":"nat"}}`)
				} else {
					fmt.Fprint(w, response)
				}
			}))
			defer srv.Close()
			reply, err := (&virtualisPlugin{client: srv.Client()}).ManageHost(context.Background(), &pb.ManageHostRequest{HostId: "7", Action: pb.HostAction_HOST_ACTION_RESIZE, Resources: &pb.HostResources{Cpu: 2, MemoryMb: 1024, DiskGb: 20}, InterfaceConfig: testConfig(srv, "key")})
			if err == nil && (reply.GetSuccess() || reply.GetError() == "") {
				t.Fatalf("malformed resize success=%v", reply)
			}
		})
	}
}

func TestFeatureSanitizationRejectsNestedSecretValues(t *testing.T) {
	for _, tc := range []struct{ kind, raw string }{
		{"snapshot_list", `{"items":[{"id":3,"name":{"api_key":"secret"}}]}`},
		{"backup_create", `{"id":4,"remark":{"file_path":"secret"}}`},
		{"instance", `{"id":7,"network":{"mode":{"password":"secret"}}}`},
		{"firewall_list", `{"items":[{"id":5,"cidr":{"token":"secret"}}]}`},
		{"trash_list", `{"items":[],"total":{"api_key":"secret"}}`},
		{"batch", `{"ok":[7],"failed":[{"id":8,"reason":{"token":"secret"}}]}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			data, err := sanitizeFeatureData([]byte(tc.raw), tc.kind)
			if err == nil && strings.Contains(string(data), "secret") {
				t.Fatalf("nested secret escaped allowlist: %s", data)
			}
		})
	}
}

func TestFeatureResultsRedactInterfaceCredentialsInSafeFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"items":[{"id":4,"name":"backup","remark":"upstream-key https://private.example/"}]}`)
	}))
	defer srv.Close()
	reply, err := (&virtualisPlugin{client: srv.Client()}).HostOperation(context.Background(), &pb.HostOperationRequest{HostId:"7", Action:"backup_list", InterfaceConfig:testConfig(srv,"upstream-key")})
	if err != nil || reply.GetError() != "" || strings.Contains(reply.GetDataJson(), "upstream-key") {
		t.Fatalf("credential escaped result sanitizer: %v %v",reply,err)
	}
}

func TestHTTPFailureDoesNotExposeUpstreamSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprintf(w, `{"code":%q,"message":%q}`, "private-key", r.Host+" private-key")
	}))
	defer srv.Close()
	creds, _ := credsFromConfig(testConfig(srv, "private-key"))
	err := (&virtualisPlugin{client: srv.Client()}).apiGet(context.Background(), creds, "/images", nil)
	if err == nil || strings.Contains(err.Error(), "private-key") {
		t.Fatalf("secret in error: %v", err)
	}
	base := srv.URL
	srv.Close()
	creds, _ = credsFromConfig(map[string]string{"api_url": base + "/private-path", "api_key": "private-key"})
	err = (&virtualisPlugin{}).apiGet(context.Background(), creds, "/images", nil)
	if err == nil || strings.Contains(err.Error(), base) || strings.Contains(err.Error(), "private-path") {
		t.Fatalf("upstream URL in error: %v", err)
	}
}
