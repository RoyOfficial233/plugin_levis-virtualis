package main

// Contract tests pinning the plugin to the *real* upstream Virtualis wire
// literals (see virtualis/internal/model/virtualis.go status constants), not
// to Levis-side mock states. These documents the cross-repo contract agreed
// with the Levis host: resize is advertised, upstream `stopped` reaches Levis
// as `stopped`, and recovery-point statuses pass through unchanged.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// upstreamInstanceJSON renders an instance exactly like the real master does
// for the given status literal.
func upstreamInstanceJSON(statusLit string) string {
	return fmt.Sprintf(`{"id":7,"name":"demo","driver":"incus","type":"elastic","status":%q,"spec":{"cpu":1,"memory_mb":512,"disk_gb":10},"network":{"mode":"nat","bandwidth_mbps":10,"traffic_gb":100}}`, statusLit)
}

func TestGetHostContractUsesUpstreamStatusLiterals(t *testing.T) {
	// Upstream literals come from virtualis model.InstanceStatus*; expected
	// values are the Levis service states. `stopped` must pass through so the
	// Levis change flow (which only accepts stopped/off) can ever trigger.
	for _, tc := range []struct{ upstream, want string }{
		{"running", "active"},
		{"stopped", "stopped"},
		{"error", "suspended"},
		{"creating", "pending"},
		{"pending", "pending"},
		{"suspended", "pending"},
		{"deleting", "pending"},
	} {
		t.Run(tc.upstream, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/instances/7":
					fmt.Fprint(w, upstreamInstanceJSON(tc.upstream))
				case "/api/v1/instances/7/access":
					fmt.Fprint(w, `{"network":{"mode":"nat","ipv4":"192.0.2.7"},"ssh":{"host":"192.0.2.7","port":2222,"username":"root","ready":true}}`)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			reply, err := (&virtualisPlugin{client: srv.Client()}).GetHost(context.Background(), &pb.GetHostRequest{HostId: "7", InterfaceConfig: testConfig(srv, "contract-key")})
			if err != nil || reply.GetError() != "" {
				t.Fatalf("reply=%v err=%v", reply, err)
			}
			if got := reply.GetHost().GetStatus(); got != tc.want {
				t.Fatalf("upstream %q mapped to %q, want %q", tc.upstream, got, tc.want)
			}
		})
	}
}

func TestGetHostContractAdvertisesResize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instances/7" {
			fmt.Fprint(w, upstreamInstanceJSON("stopped"))
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	reply, err := (&virtualisPlugin{client: srv.Client()}).GetHost(context.Background(), &pb.GetHostRequest{HostId: "7", InterfaceConfig: testConfig(srv, "contract-key")})
	if err != nil || reply.GetError() != "" {
		t.Fatalf("reply=%v err=%v", reply, err)
	}
	// The upstream master has shipped PATCH /api/v1/instances/:id/spec, so the
	// plugin can always advertise resize; Levis gates plan changes on it.
	got := reply.GetHost().GetActions()
	want := map[string]bool{"boot": true, "shutdown": true, "reboot": true, "hard_boot": true, "hard_stop": true, "hard_restart": true, "reinstall": true, "resize": true}
	seen := map[string]bool{}
	for _, action := range got {
		if !want[action] {
			t.Fatalf("unexpected advertised action %q in %v", action, got)
		}
		seen[action] = true
	}
	if len(seen) != len(want) {
		t.Fatalf("advertised actions %v missing entries of %v", got, want)
	}
}

func TestGetOrderContractMapsUpstreamStatusLiterals(t *testing.T) {
	for _, tc := range []struct{ upstream, want string }{
		{"running", "active"},
		{"stopped", "stopped"},
		{"error", "suspended"},
		{"creating", "pending"},
	} {
		t.Run(tc.upstream, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, upstreamInstanceJSON(tc.upstream))
			}))
			defer srv.Close()
			reply, err := (&virtualisPlugin{client: srv.Client()}).GetOrder(context.Background(), &pb.GetOrderRequest{UpstreamOrderId: "7", InterfaceConfig: testConfig(srv, "contract-key")})
			if err != nil || reply.GetError() != "" {
				t.Fatalf("reply=%v err=%v", reply, err)
			}
			if got := reply.GetStatus(); got != tc.want {
				t.Fatalf("upstream %q mapped to %q, want %q", tc.upstream, got, tc.want)
			}
		})
	}
}

// The frontend keys recovery controls on the upstream success literal
// `available`; the plugin must forward status strings verbatim.
func TestRecoveryListPassthroughPreservesUpstreamStatusLiterals(t *testing.T) {
	for _, tc := range []struct{ action, lit string }{
		{"snapshot_list", "available"},
		{"snapshot_list", "creating"},
		{"snapshot_list", "error"},
		{"backup_list", "available"},
		{"backup_list", "error"},
	} {
		t.Run(tc.action+"/"+tc.lit, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"items":[{"id":4,"instance_id":7,"name":"snap","size_bytes":16,"status":%q,"created_at":"2026-10-08T00:00:00Z"}]}`, tc.lit)
			}))
			defer srv.Close()
			reply, err := (&virtualisPlugin{client: srv.Client()}).HostOperation(context.Background(), &pb.HostOperationRequest{HostId: "7", Action: tc.action, InterfaceConfig: testConfig(srv, "contract-key")})
			if err != nil || reply.GetError() != "" {
				t.Fatalf("reply=%v err=%v", reply, err)
			}
			if !strings.Contains(reply.GetDataJson(), fmt.Sprintf(`"status":%q`, tc.lit)) {
				t.Fatalf("upstream status literal %q not preserved: %s", tc.lit, reply.GetDataJson())
			}
		})
	}
}

// Long-running mutations (disk replacement, permanent purge, reinstall) get
// the 2h recovery deadline; ordinary power operations keep the short budget.
func TestManageTimeoutClassifiesLongRunningMutations(t *testing.T) {
	for _, action := range []pb.HostAction{
		pb.HostAction_HOST_ACTION_RESIZE,
		pb.HostAction_HOST_ACTION_TERMINATE,
		pb.HostAction_HOST_ACTION_REINSTALL,
	} {
		if got := manageTimeout(action); got != 2*time.Hour {
			t.Fatalf("manageTimeout(%v) = %v, want %v", action, got, recoveryTimeout)
		}
	}
	for _, action := range []pb.HostAction{
		pb.HostAction_HOST_ACTION_BOOT,
		pb.HostAction_HOST_ACTION_SHUTDOWN,
		pb.HostAction_HOST_ACTION_REBOOT,
		pb.HostAction_HOST_ACTION_SUSPEND,
		pb.HostAction_HOST_ACTION_UNSUSPEND,
		pb.HostAction_HOST_ACTION_HARD_BOOT,
		pb.HostAction_HOST_ACTION_HARD_STOP,
		pb.HostAction_HOST_ACTION_HARD_RESTART,
		pb.HostAction_HOST_ACTION_RENEW,
		pb.HostAction_HOST_ACTION_UNSPECIFIED,
	} {
		if got := manageTimeout(action); got != requestTimeout {
			t.Fatalf("manageTimeout(%v) = %v, want %v", action, got, requestTimeout)
		}
	}
}

func TestHostOperationPlansClassifyTimeouts(t *testing.T) {
	for _, tc := range []struct {
		action, payload string
		want            time.Duration
	}{
		{"snapshot_list", `{}`, requestTimeout},
		{"backup_list", `{}`, requestTimeout},
		{"firewall_list", `{}`, requestTimeout},
		{"vpc_list", `{}`, requestTimeout},
		{"security_groups_list", `{}`, requestTimeout},
		{"snapshot_create", `{"name":"snap"}`, recoveryTimeout},
		{"backup_create", `{"name":"bak"}`, recoveryTimeout},
		{"snapshot_restore", `{"snapshot_id":3}`, recoveryTimeout},
		{"snapshot_delete", `{"snapshot_id":3}`, recoveryTimeout},
		{"backup_restore", `{"backup_id":4}`, recoveryTimeout},
		{"backup_delete", `{"backup_id":4}`, recoveryTimeout},
		{"migrate", `{"target_agent_id":2}`, recoveryTimeout},
		{"trash_restore", `{}`, recoveryTimeout},
		{"trash_purge", `{}`, recoveryTimeout},
		{"recycle", `{}`, recoveryTimeout},
		{"batch", `{"ids":[7],"action":"stop"}`, recoveryTimeout},
	} {
		t.Run(tc.action, func(t *testing.T) {
			plan, err := planHostOperation("7", tc.action, tc.payload)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if plan.timeout != tc.want {
				t.Fatalf("plan.timeout = %v, want %v", plan.timeout, tc.want)
			}
		})
	}
}

// PLG-F05: every ManageHost mutation must stamp the Levis operation ID header
// so upstream logs can be reconciled; empty/invalid IDs must be omitted.
func TestTerminateCarriesLevisOperationID(t *testing.T) {
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/instances/7/purge" {
			t.Errorf("route = %s %s", r.Method, r.URL.Path)
		}
		header = r.Header.Get("X-Levis-Operation-ID")
		w.WriteHeader(204)
	}))
	defer srv.Close()
	reply, err := (&virtualisPlugin{client: srv.Client()}).ManageHost(context.Background(), &pb.ManageHostRequest{
		HostId: "7", Action: pb.HostAction_HOST_ACTION_TERMINATE, OperationId: "terminate-42",
		InterfaceConfig: testConfig(srv, "key"),
	})
	if err != nil || !reply.GetSuccess() {
		t.Fatalf("reply=%v err=%v", reply, err)
	}
	if header != "terminate-42" {
		t.Fatalf("X-Levis-Operation-ID = %q, want terminate-42", header)
	}
}

func TestOperationIDHeaderOmittedWhenMissingOrInvalid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opID   string
		action pb.HostAction
	}{
		{"terminate_empty", "", pb.HostAction_HOST_ACTION_TERMINATE},
		{"terminate_invalid", "bad\r\nid", pb.HostAction_HOST_ACTION_TERMINATE},
		{"reboot_empty", "", pb.HostAction_HOST_ACTION_REBOOT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var header, method, path string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				header, method, path = r.Header.Get("X-Levis-Operation-ID"), r.Method, r.URL.Path
				w.WriteHeader(204)
			}))
			defer srv.Close()
			reply, err := (&virtualisPlugin{client: srv.Client()}).ManageHost(context.Background(), &pb.ManageHostRequest{
				HostId: "7", Action: tc.action, OperationId: tc.opID,
				InterfaceConfig: testConfig(srv, "key"),
			})
			if err != nil || !reply.GetSuccess() {
				t.Fatalf("reply=%v err=%v", reply, err)
			}
			if header != "" {
				t.Fatalf("%s %s sent X-Levis-Operation-ID %q", method, path, header)
			}
		})
	}
}

func TestReinstallCarriesLevisOperationID(t *testing.T) {
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("X-Levis-Operation-ID")
		w.WriteHeader(204)
	}))
	defer srv.Close()
	reply, err := (&virtualisPlugin{client: srv.Client()}).ManageHost(context.Background(), &pb.ManageHostRequest{
		HostId: "7", Action: pb.HostAction_HOST_ACTION_REINSTALL, Os: "9", OperationId: "reinstall-7",
		InterfaceConfig: testConfig(srv, "key"),
	})
	if err != nil || !reply.GetSuccess() {
		t.Fatalf("reply=%v err=%v", reply, err)
	}
	if header != "reinstall-7" {
		t.Fatalf("X-Levis-Operation-ID = %q, want reinstall-7", header)
	}
}

// PLG-F06 helper: the reply error string must keep the HTTP status and the
// structured upstream code so the Levis host can map failure categories.
func TestUpstreamErrorCategorySurvivesToReply(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{429, `{"code":"RATE_LIMITED","message":"slow down"}`},
		{409, `{"code":"CONFLICT","message":"busy"}`},
		{403, `{"code":"FORBIDDEN","message":"denied"}`},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			reply, err := (&virtualisPlugin{client: srv.Client()}).HostOperation(context.Background(), &pb.HostOperationRequest{HostId: "7", Action: "snapshot_list", InterfaceConfig: testConfig(srv, "key")})
			if err != nil || reply.GetError() == "" {
				t.Fatalf("reply=%v err=%v", reply, err)
			}
			msg := reply.GetError()
			if !strings.Contains(msg, fmt.Sprint(tc.status)) {
				t.Fatalf("error lost HTTP status: %q", msg)
			}
			code := strings.SplitN(strings.SplitN(msg, "(", 2)[1], ")", 2)[0]
			if !strings.Contains(tc.body, code) || code == "" {
				t.Fatalf("error lost structured code: %q", msg)
			}
		})
	}
}

// A transport deadline must surface as DeadlineExceeded (not an opaque
// string) so the host can distinguish retry-later from unreachable.
func TestTransportTimeoutKeepsDeadlineCategory(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)
	creds, err := credsFromConfig(testConfig(srv, "key"))
	if err != nil {
		t.Fatal(err)
	}
	err = (&virtualisPlugin{client: srv.Client()}).apiDoTimeout(context.Background(), http.MethodGet, creds, "/images", nil, nil, 50*time.Millisecond)
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("timeout error = %v, want DeadlineExceeded", err)
	}
	// The reply.Error text is what Levis actually maps on; the gRPC category
	// name must survive into it.
	if !strings.Contains(err.Error(), "DeadlineExceeded") {
		t.Fatalf("category name lost from error text: %q", err.Error())
	}
}
