package main

import (
	"fmt"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

var interfacePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

func validateNetwork(n v1Network) error {
	if n.DedicatedMode != "" && n.DedicatedMode != "auto" && n.DedicatedMode != "routed" && n.DedicatedMode != "bridge" {
		return fmt.Errorf("独立网络模式仅支持 auto/routed/bridge")
	}
	if n.DedicatedMode != "" && n.Mode != "dedicated" {
		return fmt.Errorf("dedicated_mode 仅适用于独立网络")
	}
	if n.Mode == "none" && (n.Bridge != "" || n.MAC != "" || n.IPv4 != "" || n.Gateway != "" || len(n.DNS) > 0) {
		return fmt.Errorf("无网络模式不能携带网卡配置")
	}
	if n.Mode != "nat" && n.Mode != "dedicated" && n.Mode != "vpc" && n.Mode != "none" {
		return fmt.Errorf("网络模式仅支持 nat/dedicated/vpc/none")
	}
	if n.Bridge != "" && (!interfacePattern.MatchString(n.Bridge) || n.Bridge == "." || n.Bridge == "..") {
		return fmt.Errorf("网络接口名称无效")
	}
	if n.MAC != "" {
		m, err := net.ParseMAC(n.MAC)
		if err != nil || len(m) != 6 {
			return fmt.Errorf("MAC 地址无效")
		}
	}
	if n.IPv4 != "" {
		ip := net.ParseIP(n.IPv4)
		if strings.Contains(n.IPv4, "/") {
			ip, _, _ = net.ParseCIDR(n.IPv4)
		}
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("IPv4 地址无效")
		}
	}
	if n.Gateway != "" {
		ip := net.ParseIP(n.Gateway)
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("网关地址无效")
		}
	}
	if len(n.DNS) > 4 {
		return fmt.Errorf("最多配置 4 个 DNS 地址")
	}
	for _, dns := range n.DNS {
		if net.ParseIP(dns) == nil {
			return fmt.Errorf("DNS 地址无效")
		}
	}
	if n.BandwidthMbps < 0 || n.BandwidthMbps > 100000 || n.TrafficGB < 0 || n.TrafficGB > 102400 {
		return fmt.Errorf("带宽或流量越界")
	}
	return nil
}

func strictOptionInt(options map[string]string, key string, def int64) (int64, error) {
	raw, ok := options[key]
	if !ok || raw == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != raw {
		return 0, fmt.Errorf("选项 %s 必须是规范的非负整数", key)
	}
	return n, nil
}

func createPayload(options map[string]string, remark string) (map[string]any, string, error) {
	driver, err := parseDriver(options)
	if err != nil {
		return nil, "", err
	}
	cpuMilli := int32(1000)
	if raw := options["cpu"]; raw != "" {
		cpu, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(cpu) || math.IsInf(cpu, 0) || cpu < 0.1 || cpu > 64 || raw != strings.TrimSpace(raw) || math.Abs(cpu*1000-math.Round(cpu*1000)) > 0.00001 {
			return nil, "", fmt.Errorf("CPU 需在 0.1-64 核之间，最多精确到毫核")
		}
		cpuMilli = int32(math.Round(cpu * 1000))
	}
	r := &pb.HostResources{CpuMilli: cpuMilli}
	values := map[string]int64{}
	defaults := map[string]int64{"memory_mb": 512, "disk_gb": 10, "bandwidth_mbps": 0, "traffic_gb": 0, "max_nat_mappings": 0}
	for key, def := range defaults {
		n, err := strictOptionInt(options, key, def)
		if err != nil {
			return nil, "", err
		}
		values[key] = n
	}
	r.MemoryMb, r.DiskGb, r.BandwidthMbps, r.TrafficGb = values["memory_mb"], values["disk_gb"], values["bandwidth_mbps"], values["traffic_gb"]
	spec, err := resourceSpec(r)
	if err != nil {
		return nil, "", err
	}
	if arch := options["arch"]; arch != "" {
		if arch != "x86_64" && arch != "aarch64" {
			return nil, "", fmt.Errorf("架构仅支持 x86_64/aarch64")
		}
		spec.Arch = arch
	}
	n := v1Network{Mode: options["network_mode"], DedicatedMode: options["dedicated_mode"], Bridge: options["network_bridge"], MAC: options["network_mac"], IPv4: options["network_ipv4"], Gateway: options["network_gateway"], BandwidthMbps: int(r.BandwidthMbps), TrafficGB: int(r.TrafficGb)}
	if dns := options["network_dns"]; dns != "" {
		for _, s := range strings.Split(dns, ",") {
			n.DNS = append(n.DNS, strings.TrimSpace(s))
		}
	}
	ids := map[string]uint64{}
	for _, key := range []string{"image_id", "agent_id", "vpc_id", "ip_pool_entry_id"} {
		value := options[key]
		if value == "" || value == "0" {
			continue
		}
		if !validID(value) {
			return nil, "", fmt.Errorf("选项 %s ID 无效", key)
		}
		ids[key], _ = strconv.ParseUint(value, 10, 64)
	}
	if ids["vpc_id"] > 0 && ids["ip_pool_entry_id"] > 0 {
		return nil, "", fmt.Errorf("VPC 与独立 IP 池不能同时指定")
	}
	if ids["ip_pool_entry_id"] > 0 && n.IPv4 != "" {
		return nil, "", fmt.Errorf("IP 池与手工 IPv4 不能同时指定")
	}
	if n.Mode == "" {
		n.Mode = "nat"
		if ids["vpc_id"] > 0 {
			n.Mode = "vpc"
		}
		if ids["ip_pool_entry_id"] > 0 {
			n.Mode = "dedicated"
		}
	}
	if (ids["vpc_id"] > 0 || ids["ip_pool_entry_id"] > 0) && ids["agent_id"] == 0 {
		return nil, "", fmt.Errorf("选择 VPC/IP 池时必须指定所属 agent_id")
	}
	if (ids["vpc_id"] > 0 && n.Mode != "vpc") || (n.Mode == "vpc" && ids["vpc_id"] == 0) || (ids["ip_pool_entry_id"] > 0 && n.Mode != "dedicated") {
		return nil, "", fmt.Errorf("网络模式与 VPC/IP 池选择不一致")
	}
	if err := validateNetwork(n); err != nil {
		return nil, "", err
	}
	if values["max_nat_mappings"] > 10000 {
		return nil, "", fmt.Errorf("NAT 条数上限越界")
	}
	body := map[string]any{"name": instanceName(remark), "driver": driver, "type": "container", "spec": spec, "network": n}
	if driver == "qemu" {
		body["type"] = "vm"
	}
	for key, id := range ids {
		body[key] = id
	}
	if values["max_nat_mappings"] > 0 {
		body["max_nat_mappings"] = values["max_nat_mappings"]
	}
	if raw, present := options["security_group_ids"]; present {
		groupIDs := []uint64{}
		if raw != "" {
			for _, id := range strings.Split(raw, ",") {
				if !validID(id) {
					return nil, "", fmt.Errorf("安全组 ID 必须为规范的正整数 CSV")
				}
				value, _ := strconv.ParseUint(id, 10, 64)
				groupIDs = append(groupIDs, value)
			}
		}
		if err := validateGroupIDs(groupIDs); err != nil {
			return nil, "", err
		}
		body["security_group_ids"] = groupIDs
	}
	return body, driver, nil
}
