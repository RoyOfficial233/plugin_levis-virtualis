package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func globalOperation(action string) bool {
	switch action {
	case "vpc_list", "vpc_create", "vpc_delete", "ip_pool_free", "trash_list", "batch":
		return true
	}
	return false
}

func planNetworkOperation(host, action string, m map[string]json.RawMessage) (hostOperationPlan, error) {
	plan := hostOperationPlan{timeout: requestTimeout}
	base := "/instances/" + host
	switch action {
	case "firewall_list":
		if err := allowedKeys(m); err != nil {
			return plan, err
		}
		plan.method, plan.path, plan.result = http.MethodGet, base+"/firewall", "firewall_list"
	case "firewall_create", "firewall_update":
		fields := []string{"direction", "action", "protocol", "port_start", "port_end", "cidr", "priority", "enabled", "remark"}
		if action == "firewall_update" {
			fields = append(fields, "rule_id")
		}
		if err := allowedKeys(m, fields...); err != nil {
			return plan, err
		}
		input, err := firewallPayload(m)
		if err != nil {
			return plan, err
		}
		plan.method, plan.path, plan.body, plan.result = http.MethodPost, base+"/firewall", input, "firewall"
		if action == "firewall_update" {
			id, err := payloadID(m, "rule_id", true)
			if err != nil {
				return plan, err
			}
			plan.method, plan.path, plan.ruleID = http.MethodPatch, "/firewall/"+decimalID(id), id
		}
	case "firewall_delete":
		if err := allowedKeys(m, "rule_id"); err != nil {
			return plan, err
		}
		id, err := payloadID(m, "rule_id", true)
		if err != nil {
			return plan, err
		}
		plan.method, plan.path, plan.result, plan.ruleID = http.MethodDelete, "/firewall/"+decimalID(id), "empty", id
	case "vpc_list", "ip_pool_free":
		if err := allowedKeys(m, "agent_id"); err != nil {
			return plan, err
		}
		id, err := payloadID(m, "agent_id", action == "ip_pool_free")
		if err != nil {
			return plan, err
		}
		plan.method, plan.path, plan.result = http.MethodGet, "/vpcs", "vpc_list"
		if id > 0 {
			plan.path += "?agent_id=" + decimalID(id)
		}
		if action == "ip_pool_free" {
			plan.path, plan.result = "/ip-pools/"+decimalID(id)+"/free", "ip_pool_list"
		}
	case "vpc_create":
		if err := allowedKeys(m, "agent_id", "name", "driver", "subnet", "gateway", "dhcp_start", "dhcp_end", "nat", "dns", "note"); err != nil {
			return plan, err
		}
		input, err := vpcPayload(m)
		if err != nil {
			return plan, err
		}
		plan.method, plan.path, plan.body, plan.result = http.MethodPost, "/vpcs", input, "vpc"
	case "vpc_delete":
		if err := allowedKeys(m, "vpc_id"); err != nil {
			return plan, err
		}
		id, err := payloadID(m, "vpc_id", true)
		if err != nil {
			return plan, err
		}
		plan.method, plan.path, plan.result = http.MethodDelete, "/vpcs/"+decimalID(id), "empty"
	case "migrate":
		if err := allowedKeys(m, "target_agent_id", "vpc_id", "ip_pool_entry_id", "network"); err != nil {
			return plan, err
		}
		target, err := payloadID(m, "target_agent_id", true)
		if err != nil {
			return plan, err
		}
		vpc, err := payloadID(m, "vpc_id", false)
		if err != nil {
			return plan, err
		}
		pool, err := payloadID(m, "ip_pool_entry_id", false)
		if err != nil {
			return plan, err
		}
		if vpc > 0 && pool > 0 {
			return plan, fmt.Errorf("目标 VPC 与独立 IP 池不能同时指定")
		}
		body := map[string]any{"target_agent_id": target}
		if vpc > 0 {
			body["vpc_id"] = vpc
		}
		if pool > 0 {
			body["ip_pool_entry_id"] = pool
		}
		if raw, ok := m["network"]; ok {
			nested, err := decodePayload(string(raw))
			if err != nil {
				return plan, err
			}
			if err := allowedKeys(nested, networkFields...); err != nil {
				return plan, err
			}
			var n v1Network
			if err := json.Unmarshal(raw, &n); err != nil {
				return plan, fmt.Errorf("network 字段类型无效")
			}
			if err := validateNetwork(n); err != nil {
				return plan, err
			}
			if (vpc > 0 && n.Mode != "vpc") || (pool > 0 && n.Mode != "dedicated") {
				return plan, fmt.Errorf("目标网络模式与选择不一致")
			}
			body["network"] = nested
		}
		plan.method, plan.path, plan.body, plan.result, plan.timeout = http.MethodPost, base+"/migrate", body, "instance", recoveryTimeout
	case "recycle", "trash_restore", "trash_purge":
		if err := allowedKeys(m); err != nil {
			return plan, err
		}
		plan.method, plan.path, plan.result, plan.timeout = http.MethodDelete, base, "empty", recoveryTimeout
		if action == "trash_restore" {
			plan.method, plan.path, plan.result = http.MethodPost, "/trash/"+host+"/restore", "instance"
		}
		if action == "trash_purge" {
			plan.path = "/trash/" + host
		}
	case "trash_list":
		if err := allowedKeys(m, "page", "page_size"); err != nil {
			return plan, err
		}
		page, size := 1, 20
		if err := payloadValue(m, "page", &page, false); err != nil {
			return plan, err
		}
		if err := payloadValue(m, "page_size", &size, false); err != nil {
			return plan, err
		}
		if page < 1 || page > 1000000 || size < 1 || size > 100 {
			return plan, fmt.Errorf("分页参数越界")
		}
		plan.method, plan.path, plan.result = http.MethodGet, fmt.Sprintf("/trash?page=%d&page_size=%d", page, size), "trash_list"
	case "batch":
		if err := allowedKeys(m, "ids", "action"); err != nil {
			return plan, err
		}
		var ids []uint64
		var action string
		if err := payloadValue(m, "ids", &ids, true); err != nil {
			return plan, err
		}
		if err := payloadValue(m, "action", &action, true); err != nil {
			return plan, err
		}
		if len(ids) < 1 || len(ids) > 100 {
			return plan, fmt.Errorf("批量操作须选择 1-100 个实例")
		}
		if action != "start" && action != "stop" && action != "restart" && action != "delete" {
			return plan, fmt.Errorf("批量操作仅允许 start/stop/restart/delete")
		}
		unique := make([]uint64, 0, len(ids))
		seen := map[uint64]bool{}
		for _, id := range ids {
			if id == 0 {
				return plan, fmt.Errorf("批量 ID 无效")
			}
			if !seen[id] {
				seen[id] = true
				unique = append(unique, id)
			}
		}
		plan.method, plan.path, plan.body, plan.result, plan.timeout = http.MethodPost, "/instances/batch", map[string]any{"ids": unique, "action": action}, "batch", recoveryTimeout
	default:
		return plan, fmt.Errorf("不支持的 provider action；下载仅支持 DownloadHostBackup 流式 RPC")
	}
	return plan, nil
}

func (p *virtualisPlugin) verifyFirewallRule(ctx context.Context, creds *credentials, host string, ruleID uint64) error {
	var page struct {
		Items []struct {
			ID         uint64 `json:"id"`
			InstanceID uint64 `json:"instance_id"`
		} `json:"items"`
	}
	if err := p.apiGet(ctx, creds, "/instances/"+host+"/firewall", &page); err != nil {
		return err
	}
	for _, rule := range page.Items {
		if rule.ID == ruleID && decimalID(rule.InstanceID) == host {
			return nil
		}
	}
	return status.Error(codes.PermissionDenied, "规则不属于当前实例，拒绝跨实例修改")
}

type firewallInput struct {
	Direction string `json:"direction"`
	Action    string `json:"action"`
	Protocol  string `json:"protocol"`
	PortStart int    `json:"port_start"`
	PortEnd   int    `json:"port_end"`
	CIDR      string `json:"cidr"`
	Priority  int    `json:"priority"`
	Enabled   bool   `json:"enabled"`
	Remark    string `json:"remark"`
}

func firewallPayload(m map[string]json.RawMessage) (firewallInput, error) {
	r := firewallInput{Enabled: true}
	for key, value := range map[string]any{"direction": &r.Direction, "action": &r.Action, "protocol": &r.Protocol, "port_start": &r.PortStart, "port_end": &r.PortEnd, "cidr": &r.CIDR, "priority": &r.Priority, "enabled": &r.Enabled, "remark": &r.Remark} {
		if err := payloadValue(m, key, value, key == "direction" || key == "action" || key == "protocol"); err != nil {
			return r, err
		}
	}
	if (r.Direction != "in" && r.Direction != "out") || (r.Action != "accept" && r.Action != "drop") || (r.Protocol != "tcp" && r.Protocol != "udp" && r.Protocol != "icmp" && r.Protocol != "any") {
		return r, fmt.Errorf("防火墙方向/动作/协议无效")
	}
	if r.PortStart < 0 || r.PortStart > 65535 || r.PortEnd < 0 || r.PortEnd > 65535 || r.Priority < 0 || r.Priority > 65535 || !textValid(r.Remark, 255) {
		return r, fmt.Errorf("防火墙端口/优先级/备注无效")
	}
	if r.PortStart == 0 && r.PortEnd != 0 {
		return r, fmt.Errorf("必须指定起始端口")
	}
	if r.PortStart > 0 {
		if r.Protocol != "tcp" && r.Protocol != "udp" {
			return r, fmt.Errorf("仅 TCP/UDP 支持端口")
		}
		if r.PortEnd == 0 {
			r.PortEnd = r.PortStart
		}
		if r.PortEnd < r.PortStart {
			return r, fmt.Errorf("端口范围无效")
		}
	}
	if r.CIDR != "" {
		ip := net.ParseIP(r.CIDR)
		if ip != nil && ip.To4() != nil {
			r.CIDR += "/32"
		}
		ip, _, err := net.ParseCIDR(r.CIDR)
		if err != nil || ip.To4() == nil {
			return r, fmt.Errorf("防火墙 CIDR 无效")
		}
	}
	return r, nil
}

type vpcInput struct {
	AgentID   uint64   `json:"agent_id"`
	Name      string   `json:"name"`
	Driver    string   `json:"driver"`
	Subnet    string   `json:"subnet"`
	Gateway   string   `json:"gateway"`
	DHCPStart string   `json:"dhcp_start"`
	DHCPEnd   string   `json:"dhcp_end"`
	NAT       bool     `json:"nat"`
	DNS       []string `json:"dns"`
	Note      string   `json:"note"`
}

var vpcName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,14}$`)

func vpcPayload(m map[string]json.RawMessage) (vpcInput, error) {
	v := vpcInput{NAT: true, DNS: []string{}}
	id, err := payloadID(m, "agent_id", true)
	if err != nil {
		return v, err
	}
	v.AgentID = id
	for key, value := range map[string]any{"name": &v.Name, "driver": &v.Driver, "subnet": &v.Subnet, "gateway": &v.Gateway, "dhcp_start": &v.DHCPStart, "dhcp_end": &v.DHCPEnd, "nat": &v.NAT, "dns": &v.DNS, "note": &v.Note} {
		if err := payloadValue(m, key, value, key == "name" || key == "driver" || key == "subnet" || key == "gateway"); err != nil {
			return v, err
		}
	}
	if !vpcName.MatchString(v.Name) || (v.Driver != "incus" && v.Driver != "qemu") || !textValid(v.Note, 255) {
		return v, fmt.Errorf("VPC 名称/驱动/备注无效")
	}
	ip, block, err := net.ParseCIDR(v.Subnet)
	if err != nil || ip.To4() == nil {
		return v, fmt.Errorf("VPC 子网无效")
	}
	prefix, bits := block.Mask.Size()
	if bits != 32 || prefix < 16 || prefix > 29 {
		return v, fmt.Errorf("VPC 子网需为 IPv4 /16-/29")
	}
	usable := func(raw string) bool {
		ip := net.ParseIP(raw)
		if ip == nil || ip.To4() == nil || !block.Contains(ip) || ip.Equal(block.IP) {
			return false
		}
		last := append(net.IP(nil), block.IP...)
		for i := range last {
			last[i] |= ^block.Mask[i]
		}
		return !ip.Equal(last)
	}
	if !usable(v.Gateway) {
		return v, fmt.Errorf("VPC 网关须在子网可用范围")
	}
	if (v.DHCPStart == "") != (v.DHCPEnd == "") {
		return v, fmt.Errorf("DHCP 起止地址必须同时指定")
	}
	if v.DHCPStart != "" && (!usable(v.DHCPStart) || !usable(v.DHCPEnd)) {
		return v, fmt.Errorf("DHCP 地址不在可用子网内")
	}
	if err := validateNetwork(v1Network{Mode: "vpc", DNS: v.DNS}); err != nil {
		return v, err
	}
	return v, nil
}

var firewallFields = []string{"id", "instance_id", "agent_id", "direction", "action", "protocol", "port_start", "port_end", "cidr", "priority", "enabled", "remark", "created_at"}
var vpcFields = []string{"id", "agent_id", "name", "driver", "subnet", "gateway", "dhcp_start", "dhcp_end", "nat", "dns", "note", "instance_count", "created_at"}
var ipPoolFields = []string{"id", "agent_id", "ip", "prefix", "gateway", "dns", "interface", "status", "note"}

func featureFields(kind string) []string {
	switch {
	case strings.HasPrefix(kind, "firewall"):
		return firewallFields
	case strings.HasPrefix(kind, "vpc"):
		return vpcFields
	case strings.HasPrefix(kind, "ip_pool"):
		return ipPoolFields
	}
	return recoveryFields
}
