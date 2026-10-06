package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

func TestCreateOrderProvisioningNetworkOptions(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		options map[string]string
		idKey   string
		id      float64
	}{
		{"vpc", map[string]string{"driver": "incus", "cpu": "0.5", "agent_id": "3", "vpc_id": "9", "network_mode": "vpc", "network_ipv4": "10.0.0.8/24", "network_gateway": "10.0.0.1", "network_dns": "1.1.1.1,8.8.8.8"}, "vpc_id", 9},
		{"dedicated", map[string]string{"driver": "qemu", "cpu": "0.5", "agent_id": "3", "ip_pool_entry_id": "11", "network_mode": "dedicated", "network_bridge": "br0", "network_mac": "02:00:00:00:00:07"}, "ip_pool_entry_id", 11},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			tc.options["image_id"] = "4"
			tc.options["traffic_gb"] = "200"
			tc.options["bandwidth_mbps"] = "50"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/api/v1/instances" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("X-Virtualis-Api-Key") != "create-key" {
					t.Error("missing credentials")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				network := body["network"].(map[string]any)
				if body[tc.idKey] != tc.id || body["agent_id"] != float64(3) || network["mode"] != tc.mode || network["traffic_gb"] != float64(200) || network["bandwidth_mbps"] != float64(50) {
					t.Errorf("body = %+v", body)
				}
				if tc.mode == "vpc" && (network["ipv4"] != "10.0.0.8/24" || network["gateway"] != "10.0.0.1" || len(network["dns"].([]any)) != 2) {
					t.Errorf("network = %+v", network)
				}
				if tc.mode == "dedicated" && (network["bridge"] != "br0" || network["mac"] != "02:00:00:00:00:07" || body["type"] != "vm") {
					t.Errorf("body = %+v", body)
				}
				fmt.Fprint(w, `{"id":7,"name":"test"}`)
			}))
			defer srv.Close()
			p := &virtualisPlugin{client: srv.Client()}
			reply, err := p.CreateOrder(context.Background(), &pb.CreateOrderRequest{Options: tc.options, InterfaceConfig: testConfig(srv, "create-key")})
			if err != nil || reply.GetError() != "" || reply.GetUpstreamOrderId() != "7" {
				t.Fatalf("reply=%v err=%v", reply, err)
			}
		})
	}
}

func TestCreateOrderRejectsInvalidOptionsBeforeHTTP(t *testing.T) {
	for _, options := range []map[string]string{
		{"driver": "arbitrary"}, {"cpu": "NaN"}, {"cpu": "Inf"}, {"cpu": "invalid"}, {"memory_mb": "oops"}, {"disk_gb": "99999"},
		{"agent_id": "../3"}, {"image_id": "-1"}, {"vpc_id": "9"}, {"agent_id": "3", "ip_pool_entry_id": "11", "network_mode": "vpc", "vpc_id": "9"},
		{"network_mode": "evil"}, {"network_ipv4": "not-ip"}, {"network_dns": "bad"}, {"network_bridge": "../br0"}, {"bandwidth_mbps": "-1"}, {"traffic_gb": "102401"},
	} {
		t.Run(fmt.Sprint(options), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				fmt.Fprint(w, `{"items":[{"id":4}],"id":7}`)
			}))
			defer srv.Close()
			p := &virtualisPlugin{client: srv.Client()}
			reply, err := p.CreateOrder(context.Background(), &pb.CreateOrderRequest{Options: options, InterfaceConfig: testConfig(srv, "create-key")})
			if err == nil && reply.GetError() == "" {
				t.Fatal("invalid options accepted")
			}
			if calls.Load() != 0 {
				t.Fatal("invalid options reached upstream")
			}
		})
	}
}
