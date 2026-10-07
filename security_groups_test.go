package main

import (
	"context"
	"encoding/json"
	"fmt"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const safeGroup = `{"id":3,"name":"web","description":"group-key","ingress_policy":"drop","egress_policy":"accept","rules":[{"id":5,"direction":"in","action":"accept","protocol":"tcp","port_start":443,"port_end":443,"priority":1,"enabled":true,"remark":"https","shell":"secret"}],"token":"secret"}`
const safeBinding = `{"security_group_ids":[3],"groups":[` + safeGroup + `],"effective_rules":[{"id":5,"direction":"in","action":"accept","protocol":"tcp","port_start":443,"port_end":443,"priority":1,"enabled":true}],"firewall_policy":{"ingress":"drop","egress":"accept","token":"secret"},"ssh_password":"secret"}`

func TestSecurityGroupOperationsUseFixedEndpointsAndTypedResponses(t *testing.T) {
	for _, tc := range []struct{ action, host, payload, method, path, response string }{
		{"security_groups_list", "", "{}", "GET", "/api/v1/security-groups", `{"items":[` + safeGroup + `],"api_key":"secret"}`},
		{"security_groups_get", "7", "{}", "GET", "/api/v1/instances/7/security-groups", safeBinding},
		{"security_groups_set", "7", `{"security_group_ids":[3]}`, "PUT", "/api/v1/instances/7/security-groups", safeBinding},
	} {
		t.Run(tc.action, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != tc.method || r.URL.Path != tc.path {
					t.Errorf("route %s %s", r.Method, r.URL.Path)
				}
				if r.Method == "PUT" {
					var body map[string]json.RawMessage
					_ = json.NewDecoder(r.Body).Decode(&body)
					if len(body) != 1 || string(body["security_group_ids"]) != "[3]" {
						t.Errorf("body=%v", body)
					}
				}
				fmt.Fprint(w, tc.response)
			}))
			defer srv.Close()
			p := &virtualisPlugin{client: srv.Client()}
			reply, err := p.HostOperation(context.Background(), &pb.HostOperationRequest{HostId: tc.host, Action: tc.action, PayloadJson: tc.payload, InterfaceConfig: testConfig(srv, "group-key")})
			if err != nil || reply.GetError() != "" || calls != 1 {
				t.Fatalf("reply=%v err=%v calls=%d", reply, err, calls)
			}
			if strings.Contains(reply.DataJson, "secret") || strings.Contains(reply.DataJson, "group-key") {
				t.Fatal("leaked secret")
			}
			if !strings.Contains(reply.DataJson, `"ingress_policy":"drop"`) || !strings.Contains(reply.DataJson, `"port_start":443`) {
				t.Fatalf("lost effective policy/rules: %s", reply.DataJson)
			}
		})
	}
}

func TestSecurityGroupOperationsRejectUnsafePayloads(t *testing.T) {
	for _, payload := range []string{`{}`, `{"security_group_ids":null}`, `{"security_group_ids":[0]}`, `{"security_group_ids":[3,3]}`, `{"security_group_ids":["3"]}`, `{"security_group_ids":[3],"path":"/admin"}`, `{"security_group_ids":[3],"security_group_ids":[9]}`, `{"security_group_ids":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17]}`} {
		if _, err := planHostOperation("7", "security_groups_set", payload); err == nil {
			t.Errorf("accepted %s", payload)
		}
	}
	for _, host := range []string{"", "../7", "07"} {
		if _, err := planHostOperation(host, "security_groups_get", "{}"); err == nil {
			t.Errorf("accepted host %q", host)
		}
	}
}

func TestSecurityGroupEmptyBindingPreservesLegacyFirewallPolicy(t *testing.T) {
	raw := json.RawMessage(`{"security_group_ids":[],"groups":[],"effective_rules":[],"firewall_policy":null}`)
	out, err := sanitizeFeatureData(raw, "security_groups_binding")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"firewall_policy":null`) {
		t.Fatalf("invented implicit policy: %s", out)
	}
}

func TestSecurityGroupSanitizerRejectsMalformedNestedAndOversizedData(t *testing.T) {
	for _, raw := range []string{
		`{"items":null}`, `{"items":[{"id":3,"name":{"token":"secret"}}]}`,
		strings.Replace(`{"items":[`+safeGroup+`]}`, `"name":"web"`, `"name":"`+strings.Repeat("x", 65)+`"`, 1),
		strings.Replace(`{"items":[`+safeGroup+`]}`, `"rules":[`, `"rules":[`+strings.Repeat(`{"direction":"in","action":"drop","protocol":"any"},`, 256), 1),
	} {
		if _, err := sanitizeFeatureData(json.RawMessage(raw), "security_groups_list"); err == nil {
			t.Errorf("malformed upstream group accepted")
		}
	}
}
