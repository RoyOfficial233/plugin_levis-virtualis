package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var recoveryName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type hostOperationPlan struct {
	method, path, result string
	body                 any
	timeout              time.Duration
	ruleID               uint64
}

func (p *virtualisPlugin) HostOperation(ctx context.Context, req *pb.HostOperationRequest) (*pb.HostOperationReply, error) {
	creds, err := credsFromConfig(req.GetInterfaceConfig())
	if err != nil {
		return nil, err
	}
	plan, err := planHostOperation(req.GetHostId(), req.GetAction(), req.GetPayloadJson())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if plan.ruleID > 0 {
		if err := p.verifyFirewallRule(ctx, creds, req.GetHostId(), plan.ruleID); err != nil {
			return &pb.HostOperationReply{Error: err.Error()}, nil
		}
	}
	var raw json.RawMessage
	if err := p.apiDoTimeout(ctx, plan.method, creds, plan.path, plan.body, &raw, plan.timeout); err != nil {
		return &pb.HostOperationReply{Error: err.Error()}, nil
	}
	data, err := sanitizeFeatureData(raw, plan.result)
	if err != nil {
		return &pb.HostOperationReply{Error: err.Error()}, nil
	}
	return &pb.HostOperationReply{DataJson: string(redactSecrets(data, creds))}, nil
}

// This switch is the sole feature-to-HTTP mapping. Caller paths, methods, URLs,
// credentials and arbitrary HTTP actions are not accepted as payload fields.
func planHostOperation(host, action, payload string) (hostOperationPlan, error) {
	m, err := decodePayload(payload)
	if err != nil {
		return hostOperationPlan{}, err
	}
	plan := hostOperationPlan{timeout: requestTimeout}
	if (!globalOperation(action) || host != "") && !validID(host) {
		return plan, fmt.Errorf("实例 ID 无效")
	}
	base := "/instances/" + host
	switch action {
	case "snapshot_list", "backup_list":
		if err := allowedKeys(m); err != nil {
			return plan, err
		}
		resource := "snapshots"
		if action == "backup_list" {
			resource = "backups"
		}
		plan.method, plan.path, plan.result = http.MethodGet, base+"/"+resource, action
	case "snapshot_create", "backup_create":
		if err := allowedKeys(m, "name", "remark"); err != nil {
			return plan, err
		}
		var name, remark string
		if err := payloadValue(m, "name", &name, true); err != nil {
			return plan, err
		}
		if !recoveryName.MatchString(name) {
			return plan, fmt.Errorf("快照/备份名称需为 1-64 位字母、数字、下划线或连字符")
		}
		if err := payloadValue(m, "remark", &remark, false); err != nil {
			return plan, err
		}
		if !textValid(remark, 255) {
			return plan, fmt.Errorf("备注过长或含控制字符")
		}
		resource := "snapshots"
		if action == "backup_create" {
			resource = "backups"
		}
		plan.method, plan.path, plan.result, plan.timeout = http.MethodPost, base+"/"+resource, action, recoveryTimeout
		plan.body = map[string]any{"name": name, "remark": remark}
	case "snapshot_restore", "snapshot_delete", "backup_restore", "backup_delete":
		key, resource := "snapshot_id", "snapshots"
		if strings.HasPrefix(action, "backup_") {
			key, resource = "backup_id", "backups"
		}
		if err := allowedKeys(m, key); err != nil {
			return plan, err
		}
		id, err := payloadID(m, key, true)
		if err != nil {
			return plan, err
		}
		plan.path, plan.timeout = base+"/"+resource+"/"+strconv.FormatUint(id, 10), recoveryTimeout
		if strings.HasSuffix(action, "_restore") {
			plan.method, plan.path, plan.result = http.MethodPost, plan.path+"/restore", "instance"
		} else {
			plan.method, plan.result = http.MethodDelete, "empty"
		}
	default:
		return planNetworkOperation(host, action, m)
	}
	return plan, nil
}
