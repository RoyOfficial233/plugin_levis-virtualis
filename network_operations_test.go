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

func TestNetworkAndLifecycleOperationsUseFixedEndpoints(t *testing.T) {
	for _, tc := range []struct{ action, host, payload, method, path, response string }{
		{"firewall_list", "7", `{}`, "GET", "/instances/7/firewall", `{"items":[{"id":5,"instance_id":7,"direction":"in","action":"accept","protocol":"tcp","enabled":false,"token":"secret"}]}`},
		{"firewall_create", "7", `{"direction":"in","action":"accept","protocol":"tcp","port_start":22,"port_end":22,"enabled":false,"priority":1}`, "POST", "/instances/7/firewall", `{"id":5,"instance_id":7,"enabled":false}`},
		{"firewall_update", "7", `{"rule_id":5,"direction":"out","action":"drop","protocol":"any","enabled":false}`, "PATCH", "/firewall/5", `{"id":5,"instance_id":7,"enabled":false}`},
		{"firewall_delete", "7", `{"rule_id":5}`, "DELETE", "/firewall/5", ""},
		{"vpc_list", "", `{"agent_id":3}`, "GET", "/vpcs?agent_id=3", `{"items":[{"id":9,"agent_id":3,"name":"net-9","driver":"qemu","subnet":"10.0.0.0/24","instance_count":2,"token":"secret"}]}`},
		{"vpc_create", "", `{"agent_id":3,"name":"net-9","driver":"qemu","subnet":"10.0.0.0/24","gateway":"10.0.0.1","nat":false,"dns":["1.1.1.1"]}`, "POST", "/vpcs", `{"id":9,"name":"net-9","nat":false}`},
		{"vpc_delete", "", `{"vpc_id":9}`, "DELETE", "/vpcs/9", ""},
		{"ip_pool_free", "", `{"agent_id":3}`, "GET", "/ip-pools/3/free", `{"items":[{"id":11,"agent_id":3,"ip":"203.0.113.11","prefix":24,"gateway":"203.0.113.1","dns":["1.1.1.1"],"interface":"br0","status":"free","credentials":"secret"}]}`},
		{"migrate", "7", `{"target_agent_id":3,"vpc_id":9,"network":{"mode":"vpc","bandwidth_mbps":10,"traffic_gb":100}}`, "POST", "/instances/7/migrate", `{"id":7,"status":"stopped","agent_id":3,"ssh_password":"secret"}`},
		{"recycle", "7", `{}`, "DELETE", "/instances/7", ""},
		{"trash_list", "", `{"page":2,"page_size":20}`, "GET", "/trash?page=2&page_size=20", `{"items":[{"id":7,"trashed_at":"date","purge_after":"later","ssh_password":"secret"}],"total":1,"page":2,"page_size":20}`},
		{"trash_restore", "7", `{}`, "POST", "/trash/7/restore", `{"id":7,"status":"stopped","ssh_password":"secret"}`},
		{"trash_purge", "7", `{}`, "DELETE", "/trash/7", ""},
		{"batch", "", `{"ids":[7,8,7],"action":"stop"}`, "POST", "/instances/batch", `{"ok":[7],"failed":[{"id":8,"reason":"busy"}]}`},
	} {
		t.Run(tc.action, func(t *testing.T) {
			var writes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if (tc.action == "firewall_update" || tc.action == "firewall_delete") && r.Method == "GET" && r.URL.Path == "/api/v1/instances/7/firewall" {
					fmt.Fprint(w, `{"items":[{"id":5,"instance_id":7}]}`)
					return
				}
				writes.Add(1)
				if r.Method != tc.method || r.URL.RequestURI() != "/api/v1"+tc.path {
					t.Errorf("route = %s %s", r.Method, r.URL.RequestURI())
				}
				if r.Method == "POST" || r.Method == "PATCH" {
					var body map[string]any
					if r.ContentLength > 0 {
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
					}
					if strings.HasPrefix(tc.action, "firewall_") && (body["enabled"] != false || body["rule_id"] != nil) {
						t.Errorf("firewall body=%v", body)
					}
					if tc.action == "batch" && (len(body["ids"].([]any)) != 2 || body["action"] != "stop") {
						t.Errorf("batch=%v", body)
					}
					if tc.action == "migrate" && body["target_agent_id"] != float64(3) {
						t.Errorf("migration=%v", body)
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
			reply, err := p.HostOperation(context.Background(), &pb.HostOperationRequest{HostId: tc.host, Action: tc.action, PayloadJson: tc.payload, InterfaceConfig: testConfig(srv, "network-key")})
			if err != nil || reply.GetError() != "" || writes.Load() != 1 || !json.Valid([]byte(reply.GetDataJson())) {
				t.Fatalf("reply=%v err=%v calls=%d", reply, err, writes.Load())
			}
			if strings.Contains(reply.GetDataJson(), "secret") {
				t.Fatal("leaked upstream secret")
			}
			if tc.action == "trash_list" && !strings.Contains(reply.GetDataJson(), `"total":1`) {
				t.Fatal("lost pagination")
			}
			if tc.action == "batch" && !strings.Contains(reply.GetDataJson(), "busy") {
				t.Fatal("lost per-entry failure")
			}
		})
	}
}

func TestNetworkAndLifecycleRejectUnsafePayloads(t *testing.T) {
	for _, tc := range []struct{ action, payload string }{
		{"firewall_create", `{"direction":"in","action":"drop","protocol":"tcp","port_start":-1}`},
		{"firewall_update", `{"rule_id":5,"enabled":false}`}, {"firewall_create", `{"direction":"in","action":"exec","protocol":"any"}`},
		{"firewall_delete", `{"rule_id":0}`}, {"vpc_list", `{"agent_id":"3"}`}, {"vpc_create", `{"agent_id":3,"name":"../net","driver":"evil","subnet":"10.0.0.0/24","gateway":"10.0.0.1"}`},
		{"migrate", `{"target_agent_id":0}`}, {"migrate", `{"target_agent_id":3,"network":{"mode":"vpc","url":"http://evil"}}`},
		{"migrate", `{"target_agent_id":3,"network":{"mode":"dedicated","ipv4":"bad"}}`},
		{"batch", `{"ids":[7],"action":"purge"}`}, {"batch", `{"ids":[],"action":"start"}`}, {"batch", `{"ids":[7,-1],"action":"stop"}`},
		{"trash_list", `{"page_size":101}`}, {"trash_restore", `{"url":"http://evil"}`}, {"ip_pool_free", `{"agent_id":3,"path":"/admin"}`},
	} {
		t.Run(tc.action+tc.payload, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{}`) }))
			defer srv.Close()
			p := &virtualisPlugin{client: srv.Client()}
			reply, err := p.HostOperation(context.Background(), &pb.HostOperationRequest{HostId: "7", Action: tc.action, PayloadJson: tc.payload, InterfaceConfig: testConfig(srv, "key")})
			if err == nil && reply.GetError() == "" {
				t.Fatal("unsafe payload accepted")
			}
			if calls.Load() != 0 {
				t.Fatal("unsafe payload reached upstream")
			}
		})
	}
}

func TestFirewallMutationVerifiesRuleBelongsToHost(t *testing.T) {
	var mutated atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutated.Store(true)
		}
		fmt.Fprint(w, `{"items":[{"id":5,"instance_id":999}]}`)
	}))
	defer srv.Close()
	p := &virtualisPlugin{client: srv.Client()}
	reply, err := p.HostOperation(context.Background(), &pb.HostOperationRequest{HostId: "7", Action: "firewall_delete", PayloadJson: `{"rule_id":5}`, InterfaceConfig: testConfig(srv, "key")})
	if err == nil && reply.GetError() == "" {
		t.Fatal("cross-host rule deletion accepted")
	}
	if mutated.Load() {
		t.Fatal("cross-host rule was mutated")
	}
}
