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

func TestResizeMapsTypedResourcesPreservingNetworkAndArchitecture(t *testing.T) {
	var patched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Virtualis-Api-Key") != "resize-key" {
			t.Error("missing request credentials")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/instances/7":
			fmt.Fprint(w, `{"status":"stopped","driver":"qemu","spec":{"cpu":2,"memory_mb":1024,"disk_gb":10,"arch":"aarch64"},"network":{"mode":"vpc","bridge":"net-7","ipv4":"10.0.0.7/24","dns":["1.1.1.1"],"bandwidth_mbps":10,"traffic_gb":100,"future_field":"retained"}}`)
		case "PATCH /api/v1/instances/7/spec":
			patched = true
			if r.Header.Get("X-Levis-Operation-ID") != "change-123" {
				t.Error("missing retry correlation")
			}
			var body struct {
				Spec    v1Spec         `json:"spec"`
				Network map[string]any `json:"network"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Spec.CPU != 1 || body.Spec.CPUMilli != 500 || body.Spec.MemoryMB != 2048 || body.Spec.DiskGB != 20 || body.Spec.Arch != "aarch64" {
				t.Errorf("spec = %+v", body.Spec)
			}
			if body.Network["mode"] != "vpc" || body.Network["bridge"] != "net-7" || body.Network["future_field"] != "retained" || body.Network["bandwidth_mbps"] != float64(0) || body.Network["traffic_gb"] != float64(0) {
				t.Errorf("network = %+v", body.Network)
			}
			fmt.Fprint(w, `{"id":7}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	p := &virtualisPlugin{client: srv.Client()}
	reply, err := p.ManageHost(context.Background(), &pb.ManageHostRequest{HostId: "7", Action: pb.HostAction_HOST_ACTION_RESIZE, OperationId: "change-123", Resources: &pb.HostResources{Cpu: 1, CpuMilli: 500, MemoryMb: 2048, DiskGb: 20}, InterfaceConfig: testConfig(srv, "resize-key")})
	if err != nil || !reply.GetSuccess() || !patched {
		t.Fatalf("reply=%v err=%v patched=%v", reply, err, patched)
	}
}

func TestResizeRejectsInvalidResourcesAndUnsafeChanges(t *testing.T) {
	for _, tc := range []struct {
		name, current, opID string
		resources           *pb.HostResources
	}{
		{"missing", `{"status":"stopped"}`, "", nil},
		{"zero_cpu", `{"status":"stopped"}`, "", &pb.HostResources{MemoryMb: 1024, DiskGb: 10}},
		{"overflow", `{"status":"stopped"}`, "", &pb.HostResources{Cpu: 2, MemoryMb: 1 << 62, DiskGb: 10}},
		{"negative_bandwidth", `{"status":"stopped"}`, "", &pb.HostResources{Cpu: 2, MemoryMb: 1024, DiskGb: 10, BandwidthMbps: -1}},
		{"inconsistent_cpu", `{"status":"stopped"}`, "", &pb.HostResources{Cpu: 2, CpuMilli: 500, MemoryMb: 1024, DiskGb: 10}},
		{"shrink", `{"status":"stopped","spec":{"disk_gb":20}}`, "", &pb.HostResources{Cpu: 2, MemoryMb: 1024, DiskGb: 10}},
		{"running", `{"status":"running","spec":{"disk_gb":10}}`, "", &pb.HostResources{Cpu: 2, MemoryMb: 1024, DiskGb: 20}},
		{"unsafe_op", `{"status":"stopped"}`, "bad\r\nvalue", &pb.HostResources{Cpu: 2, MemoryMb: 1024, DiskGb: 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes.Add(1)
				}
				fmt.Fprint(w, tc.current)
			}))
			defer srv.Close()
			p := &virtualisPlugin{client: srv.Client()}
			reply, err := p.ManageHost(context.Background(), &pb.ManageHostRequest{HostId: "7", Action: pb.HostAction_HOST_ACTION_RESIZE, Resources: tc.resources, OperationId: tc.opID, InterfaceConfig: testConfig(srv, "resize-key")})
			if err == nil && (reply.GetSuccess() || strings.TrimSpace(reply.GetError()) == "") {
				t.Fatalf("unsafe resize accepted: %v", reply)
			}
			if writes.Load() != 0 {
				t.Fatal("unsafe resize reached PATCH")
			}
		})
	}
}

func TestInvalidHostIDsNeverReachUpstream(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	p := &virtualisPlugin{client: srv.Client()}
	config := testConfig(srv, "test-key")
	for _, id := range []string{"", "0", "01", "-1", "+1", " 1", "1 ", "1/../../trash", "1?admin=true", "%31", "18446744073709551616"} {
		t.Run(id, func(t *testing.T) {
			reply, err := p.GetHost(context.Background(), &pb.GetHostRequest{HostId: id, InterfaceConfig: config})
			if err == nil && reply.GetError() == "" {
				t.Fatalf("accepted invalid host ID %q", id)
			}
			reply2, err := p.ManageHost(context.Background(), &pb.ManageHostRequest{HostId: id, Action: pb.HostAction_HOST_ACTION_RENEW, InterfaceConfig: config})
			if err == nil && reply2.GetSuccess() {
				t.Fatalf("renew accepted invalid host ID %q", id)
			}
		})
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d invalid requests reached upstream", n)
	}
}

func TestTerminateUsesPurgeAndOnlyTreatsResourceNotFoundAsIdempotent(t *testing.T) {
	for _, tc := range []struct {
		status  int
		body    string
		success bool
	}{
		{204, "", true}, {404, `{"code":"NOT_FOUND","message":"实例不存在"}`, true},
		{404, "404 page not found", false}, {404, `{"code":"NOT_FOUND","message":"not found"}`, false}, {500, `{"message":"not found during compensation"}`, false},
		{403, `{"code":"FORBIDDEN","message":"denied"}`, false},
	} {
		t.Run(fmt.Sprint(tc.status, tc.body), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/instances/7/purge" {
					t.Errorf("terminate route = %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			p := &virtualisPlugin{client: srv.Client()}
			reply, err := p.ManageHost(context.Background(), &pb.ManageHostRequest{HostId: "7", Action: pb.HostAction_HOST_ACTION_TERMINATE, InterfaceConfig: testConfig(srv, "test-key")})
			if err != nil || reply.GetSuccess() != tc.success || (!tc.success && reply.GetError() == "") {
				t.Fatalf("reply = %v, err = %v", reply, err)
			}
		})
	}
}
