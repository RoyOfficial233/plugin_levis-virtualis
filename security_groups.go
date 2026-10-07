package main

import (
	"encoding/json"
	"fmt"
)

func validateGroupIDs(ids []uint64) error {
	if len(ids) > 16 {
		return fmt.Errorf("最多绑定 16 个安全组")
	}
	seen := map[uint64]bool{}
	for _, id := range ids {
		if id == 0 || seen[id] {
			return fmt.Errorf("安全组 ID 必须为唯一的正整数")
		}
		seen[id] = true
	}
	return nil
}

type safeGroupRule struct {
	ID              uint64 `json:"id,omitempty"`
	SecurityGroupID uint64 `json:"security_group_id,omitempty"`
	firewallInput
}
type securityGroup struct {
	ID            uint64          `json:"id"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	IngressPolicy string          `json:"ingress_policy"`
	EgressPolicy  string          `json:"egress_policy"`
	Rules         []safeGroupRule `json:"rules"`
}
type firewallPolicy struct {
	Ingress string `json:"ingress"`
	Egress  string `json:"egress"`
}
type securityGroupBinding struct {
	IDs            []uint64        `json:"security_group_ids"`
	Groups         []securityGroup `json:"groups"`
	EffectiveRules []safeGroupRule `json:"effective_rules"`
	Policy         *firewallPolicy `json:"firewall_policy"`
}

func validPolicy(value string) bool { return value == "accept" || value == "drop" }
func validateGroupRules(rules []safeGroupRule, limit int) error {
	if len(rules) > limit {
		return fmt.Errorf("上游安全组规则过多")
	}
	for _, rule := range rules {
		raw, _ := json.Marshal(rule.firewallInput)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		if _, err := firewallPayload(fields); err != nil {
			return fmt.Errorf("上游安全组规则无效: %w", err)
		}
	}
	return nil
}
func validateGroups(groups []securityGroup, limit int) error {
	if groups == nil || len(groups) > limit {
		return fmt.Errorf("上游安全组列表无效或过大")
	}
	seen := map[uint64]bool{}
	for _, group := range groups {
		if group.ID == 0 || seen[group.ID] || group.Name == "" || !textValid(group.Name, 64) || !textValid(group.Description, 1024) || !validPolicy(group.IngressPolicy) || !validPolicy(group.EgressPolicy) {
			return fmt.Errorf("上游安全组字段无效")
		}
		seen[group.ID] = true
		if err := validateGroupRules(group.Rules, 256); err != nil {
			return err
		}
	}
	return nil
}
func sanitizeSecurityGroups(raw json.RawMessage, kind string) ([]byte, error) {
	if kind == "security_groups_list" {
		var page struct {
			Items []securityGroup `json:"items"`
		}
		if json.Unmarshal(raw, &page) != nil {
			return nil, fmt.Errorf("上游安全组响应类型无效")
		}
		if err := validateGroups(page.Items, 1000); err != nil {
			return nil, err
		}
		return json.Marshal(page)
	}
	var binding securityGroupBinding
	if json.Unmarshal(raw, &binding) != nil || binding.IDs == nil || binding.EffectiveRules == nil {
		return nil, fmt.Errorf("上游安全组绑定响应无效")
	}
	if binding.Policy != nil && (!validPolicy(binding.Policy.Ingress) || !validPolicy(binding.Policy.Egress)) {
		return nil, fmt.Errorf("上游防火墙策略无效")
	}
	if len(binding.IDs) > 0 && binding.Policy == nil {
		return nil, fmt.Errorf("绑定安全组的实例缺少防火墙策略")
	}
	if err := validateGroupIDs(binding.IDs); err != nil {
		return nil, err
	}
	if err := validateGroups(binding.Groups, 16); err != nil {
		return nil, err
	}
	if len(binding.IDs) != len(binding.Groups) {
		return nil, fmt.Errorf("上游安全组绑定不一致")
	}
	seen := map[uint64]bool{}
	for _, id := range binding.IDs {
		seen[id] = true
	}
	for _, group := range binding.Groups {
		if !seen[group.ID] {
			return nil, fmt.Errorf("上游安全组绑定不一致")
		}
	}
	if err := validateGroupRules(binding.EffectiveRules, 16*256+256); err != nil {
		return nil, err
	}
	return json.Marshal(binding)
}
