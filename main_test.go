package main

import (
	"testing"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

func TestV1Status(t *testing.T) {
	cases := map[string]string{"running": "active", "stopped": "suspended", "error": "suspended", "creating": "pending"}
	for input, want := range cases {
		if got := v1Status(input); got != want {
			t.Fatalf("v1Status(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestHostMappingIncludesResourcesAccessAndHardActions(t *testing.T) {
	instance := v1Instance{
		ID: 7, Name: "demo", Driver: "incus", Status: "running",
		Spec:       v1Spec{CPU: 2, MemoryMB: 2048, DiskGB: 20},
		Network:    v1Network{Mode: "dedicated", IPv4: "203.0.113.10", BandwidthMbps: 100},
		ObservedIP: "203.0.113.10", SSHReady: true, SSHPassword: "secret",
	}
	ip := instance.ObservedIP
	host := &pb.UpstreamHost{
		Resources: &pb.HostResources{Cpu: int32(instance.Spec.CPU), MemoryMb: int64(instance.Spec.MemoryMB), DiskGb: int64(instance.Spec.DiskGB), BandwidthMbps: int64(instance.Network.BandwidthMbps)},
		Network:   &pb.HostNetwork{Mode: instance.Network.Mode, Ipv4: ip},
		Ssh:       &pb.HostSSH{Host: ip, Port: 22, Username: "root", Password: instance.SSHPassword, Ready: instance.SSHReady},
		Actions:   []string{"boot", "shutdown", "reboot", "hard_boot", "hard_stop", "hard_restart", "reinstall"},
	}
	if host.GetResources().GetCpu() != 2 || host.GetResources().GetMemoryMb() != 2048 || host.GetNetwork().GetIpv4() != "203.0.113.10" || !host.GetSsh().GetReady() {
		t.Fatalf("host mapping lost resource/network/ssh fields: %v", host)
	}
	if len(host.GetActions()) != 7 {
		t.Fatalf("actions = %v", host.GetActions())
	}
}
