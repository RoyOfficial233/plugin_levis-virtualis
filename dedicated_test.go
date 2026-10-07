package main

import (
	"context"
	"encoding/json"
	"fmt"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDedicatedOrderForwardsAutoAllocationAndGroups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if r.Method != "POST" || r.URL.Path != "/api/v1/instances" {
			t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		var network map[string]any
		_ = json.Unmarshal(body["network"], &network)
		if network["dedicated_mode"] != "routed" || network["bridge"] != "eth0" || body["ip_pool_entry_id"] != nil || network["ipv4"] != nil {
			t.Errorf("not an automatic routed allocation: %s", body["network"])
		}
		if string(body["security_group_ids"]) != "[3,9]" {
			t.Errorf("groups = %s", body["security_group_ids"])
		}
		fmt.Fprint(w, `{"id":7}`)
	}))
	defer srv.Close()
	p := &virtualisPlugin{client: srv.Client()}
	reply, err := p.CreateOrder(context.Background(), &pb.CreateOrderRequest{InterfaceConfig: testConfig(srv, "key"), Options: map[string]string{"image_id": "4", "agent_id": "3", "network_mode": "dedicated", "dedicated_mode": "routed", "network_bridge": "eth0", "security_group_ids": "3,9"}})
	if err != nil || reply.GetError() != "" {
		t.Fatalf("reply=%v err=%v", reply, err)
	}
}

func TestDedicatedOptionsRejectConflicts(t *testing.T) {
	for _, options := range []map[string]string{
		{"network_mode": "nat", "dedicated_mode": "routed"}, {"network_mode": "dedicated", "dedicated_mode": "evil"},
		{"network_mode": "none", "network_ipv4": "10.0.0.7"}, {"agent_id": "3", "ip_pool_entry_id": "11", "network_ipv4": "203.0.113.7/24"},
		{"security_group_ids": "01"}, {"security_group_ids": "3,3"}, {"security_group_ids": "3, 4"}, {"security_group_ids": "0"},
	} {
		if _, _, err := createPayload(options, "test"); err == nil {
			t.Errorf("accepted conflicting options %v", options)
		}
	}
}
