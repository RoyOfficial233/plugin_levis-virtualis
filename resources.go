package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

type operationIDKey struct{}

// withOperationID attaches the Levis operation ID to the context so every
// mutating upstream call stamps X-Levis-Operation-ID. An empty or malformed
// ID is dropped (header omitted) rather than forging correlation.
func withOperationID(ctx context.Context, id string) context.Context {
	if id == "" || !validOperationID(id) {
		return ctx
	}
	return context.WithValue(ctx, operationIDKey{}, id)
}

func validOperationID(id string) bool {
	if len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}

func resourceSpec(r *pb.HostResources) (v1Spec, error) {
	if r == nil {
		return v1Spec{}, fmt.Errorf("改配必须提供完整 typed resources")
	}
	cpu, milli := int(r.GetCpu()), int(r.GetCpuMilli())
	if milli != 0 {
		if milli < 100 || milli > 64000 {
			return v1Spec{}, fmt.Errorf("CPU 毫核需在 100-64000 之间")
		}
		derived := (milli + 999) / 1000
		if cpu != 0 && cpu != derived {
			return v1Spec{}, fmt.Errorf("cpu 与 cpu_milli 不一致")
		}
		cpu = derived
	}
	if cpu < 1 || cpu > 64 || r.GetMemoryMb() < 1 || r.GetMemoryMb() > 262144 || r.GetDiskGb() < 1 || r.GetDiskGb() > 4096 {
		return v1Spec{}, fmt.Errorf("CPU/内存/磁盘规格越界")
	}
	if r.GetBandwidthMbps() < 0 || r.GetBandwidthMbps() > 100000 || r.GetTrafficGb() < 0 || r.GetTrafficGb() > 102400 {
		return v1Spec{}, fmt.Errorf("带宽或流量规格越界")
	}
	return v1Spec{CPU: cpu, CPUMilli: milli, MemoryMB: int(r.GetMemoryMb()), DiskGB: int(r.GetDiskGb())}, nil
}

func (p *virtualisPlugin) resizeHost(ctx context.Context, creds *credentials, req *pb.ManageHostRequest) (*pb.ManageHostReply, error) {
	spec, err := resourceSpec(req.GetResources())
	if err != nil {
		return &pb.ManageHostReply{Error: err.Error()}, nil
	}
	if !validOperationID(req.GetOperationId()) {
		return &pb.ManageHostReply{Error: "operation_id 格式无效"}, nil
	}
	var current struct {
		Status  string                     `json:"status"`
		Spec    v1Spec                     `json:"spec"`
		Network map[string]json.RawMessage `json:"network"`
	}
	if err := p.apiGet(ctx, creds, "/instances/"+req.GetHostId(), &current); err != nil {
		return &pb.ManageHostReply{Error: err.Error()}, nil
	}
	// A durable retry may arrive after the customer has restarted an already
	// resized machine. If the complete requested resources match, do not issue
	// another mutation or misclassify the applied change as a remote failure.
	var bandwidth, traffic int64
	currentMilli, desiredMilli := current.Spec.CPUMilli, spec.CPUMilli
	if currentMilli == 0 {
		currentMilli = current.Spec.CPU * 1000
	}
	if desiredMilli == 0 {
		desiredMilli = spec.CPU * 1000
	}
	if current.Network != nil && json.Unmarshal(current.Network["bandwidth_mbps"], &bandwidth) == nil &&
		json.Unmarshal(current.Network["traffic_gb"], &traffic) == nil &&
		currentMilli == desiredMilli && current.Spec.MemoryMB == spec.MemoryMB && current.Spec.DiskGB == spec.DiskGB &&
		bandwidth == req.GetResources().GetBandwidthMbps() && traffic == req.GetResources().GetTrafficGb() {
		return &pb.ManageHostReply{Success: true}, nil
	}
	if current.Status != "stopped" {
		return &pb.ManageHostReply{Error: "改配前必须关机；插件不会自动断电"}, nil
	}
	if spec.DiskGB < current.Spec.DiskGB {
		return &pb.ManageHostReply{Error: "不支持缩小磁盘"}, nil
	}
	spec.Arch = current.Spec.Arch
	if current.Network == nil {
		return &pb.ManageHostReply{Error: "上游未返回完整网络配置，拒绝覆盖"}, nil
	}
	current.Network["bandwidth_mbps"] = json.RawMessage(fmt.Sprint(req.GetResources().GetBandwidthMbps()))
	current.Network["traffic_gb"] = json.RawMessage(fmt.Sprint(req.GetResources().GetTrafficGb()))
	body := map[string]any{"spec": spec, "network": current.Network}
	ctx = withOperationID(ctx, req.GetOperationId())
	var updated struct {
		ID uint64 `json:"id"`
	}
	if err := p.apiDoTimeout(ctx, http.MethodPatch, creds, "/instances/"+req.GetHostId()+"/spec", body, &updated, manageTimeout(req.GetAction())); err != nil {
		return &pb.ManageHostReply{Error: err.Error()}, nil
	}
	if decimalID(updated.ID) != req.GetHostId() {
		return &pb.ManageHostReply{Error: "上游未返回目标实例的改配结果，需核实后重试"}, nil
	}
	return &pb.ManageHostReply{Success: true}, nil
}

func parseDriver(options map[string]string) (string, error) {
	driver := options["driver"]
	if driver == "" {
		return "incus", nil
	}
	if driver != "incus" && driver != "qemu" {
		return "", fmt.Errorf("驱动仅支持 incus 或 qemu；不自动替换无效驱动")
	}
	return driver, nil
}

func textValid(value string, max int) bool {
	return len(value) <= max && strings.IndexFunc(value, func(c rune) bool { return c < 32 || c == 127 }) < 0
}
