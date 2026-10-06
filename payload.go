package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const maxOperationPayload = 64 << 10

func decodePayload(raw string) (map[string]json.RawMessage, error) {
	if raw == "" {
		raw = "{}"
	}
	if len(raw) > maxOperationPayload {
		return nil, fmt.Errorf("payload_json 超过 64 KiB")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := validateJSONValue(decoder, 0); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("JSON 含多余内容")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m == nil {
		return nil, fmt.Errorf("payload_json 必须是 JSON 对象")
	}
	return m, nil
}

// encoding/json alone accepts duplicate keys. Reject them at every nesting
// level before decoding an allowlisted payload, so validation and execution
// cannot disagree on a shadowed value.
func validateJSONValue(d *json.Decoder, depth int) error {
	if depth > 16 {
		return fmt.Errorf("JSON 嵌套过深")
	}
	token, err := d.Token()
	if err != nil {
		return fmt.Errorf("JSON 格式无效: %w", err)
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("JSON 键重复或无效")
			}
			seen[name] = true
			if err := validateJSONValue(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := validateJSONValue(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("JSON 容器无效")
	}
	_, err = d.Token()
	return err
}

func allowedKeys(m map[string]json.RawMessage, keys ...string) error {
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
	}
	for key := range m {
		if !allowed[key] {
			return fmt.Errorf("不允许的 payload 字段 %q", key)
		}
	}
	return nil
}

func payloadValue(m map[string]json.RawMessage, key string, value any, required bool) error {
	raw, ok := m[key]
	if !ok {
		if required {
			return fmt.Errorf("缺少字段 %s", key)
		}
		return nil
	}
	if string(raw) == "null" || json.Unmarshal(raw, value) != nil {
		return fmt.Errorf("字段 %s 类型无效", key)
	}
	return nil
}

func payloadID(m map[string]json.RawMessage, key string, required bool) (uint64, error) {
	var id uint64
	if err := payloadValue(m, key, &id, required); err != nil {
		return 0, err
	}
	if (required || m[key] != nil) && id == 0 {
		return 0, fmt.Errorf("字段 %s 必须是正整数 ID", key)
	}
	return id, nil
}

// Allowlisted names alone are insufficient: a malicious upstream can put a
// secret object inside a seemingly safe string field. Keep only scalar values
// and the one explicitly supported array (DNS strings).
func safeFeatureValue(key string, raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	if value == "" || !json.Valid(raw) {
		return false
	}
	if key == "dns" {
		var dns []string
		return json.Unmarshal(raw, &dns) == nil && len(dns) <= 4
	}
	return value[0] != '{' && value[0] != '['
}

func pickFields(m map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, key := range keys {
		if raw, ok := m[key]; ok && safeFeatureValue(key, raw) {
			out[key] = raw
		}
	}
	return out
}

// redactSecrets removes upstream credential material (API key, upstream URL)
// from every allowed string value; list payloads pass through the same
// sanitizer, so free-text fields like remark cannot echo secrets back.
func redactSecrets(data []byte, creds *credentials) []byte {
	if creds == nil {
		return data
	}
	text := string(data)
	for _, secret := range []string{creds.apiKey, creds.apiURL} {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	return []byte(text)
}

var recoveryFields = []string{"id", "instance_id", "agent_id", "name", "remark", "size_bytes", "status", "created_at", "driver", "checksum"}
var instanceFields = []string{"id", "agent_id", "vpc_id", "ip_pool_entry_id", "name", "display_name", "driver", "type", "status", "ip", "observed_ip", "trashed_at", "purge_after", "created_at"}
var networkFields = []string{"mode", "bridge", "mac", "ipv4", "gateway", "dns", "bandwidth_mbps", "traffic_gb"}

func sanitizeInstance(m map[string]json.RawMessage) map[string]json.RawMessage {
	out := pickFields(m, instanceFields...)
	for key, fields := range map[string][]string{"spec": {"cpu", "cpu_milli", "memory_mb", "disk_gb", "arch"}, "network": networkFields} {
		var value map[string]json.RawMessage
		if json.Unmarshal(m[key], &value) == nil && value != nil {
			out[key], _ = json.Marshal(pickFields(value, fields...))
		}
	}
	return out
}

func sanitizeFeatureData(raw json.RawMessage, kind string) ([]byte, error) {
	if kind == "empty" {
		return []byte("{}"), nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, fmt.Errorf("上游功能响应必须是 JSON 对象")
	}
	if kind == "instance" {
		if !validID(string(m["id"])) {
			return nil, fmt.Errorf("上游未返回有效实例 ID")
		}
		return json.Marshal(sanitizeInstance(m))
	}
	if kind == "batch" {
		var ok []uint64
		var failed []map[string]json.RawMessage
		if m["ok"] == nil || m["failed"] == nil || json.Unmarshal(m["ok"], &ok) != nil || json.Unmarshal(m["failed"], &failed) != nil {
			return nil, fmt.Errorf("上游未返回完整批量结果")
		}
		if ok == nil {
			ok = []uint64{}
		}
		out := make([]map[string]json.RawMessage, 0, len(failed))
		for _, item := range failed {
			out = append(out, pickFields(item, "id", "reason"))
		}
		return json.Marshal(map[string]any{"ok": ok, "failed": out})
	}
	fields := featureFields(kind)
	if strings.HasSuffix(kind, "_list") {
		var items []map[string]json.RawMessage
		if m["items"] == nil || json.Unmarshal(m["items"], &items) != nil {
			return nil, fmt.Errorf("上游未返回有效 items 列表")
		}
		out := make([]map[string]json.RawMessage, 0, len(items))
		for _, item := range items {
			if kind == "trash_list" {
				out = append(out, sanitizeInstance(item))
			} else {
				out = append(out, pickFields(item, fields...))
			}
		}
		result := map[string]any{"items": out}
		if kind == "trash_list" {
			for key, value := range pickFields(m, "total", "page", "page_size") {
				result[key] = value
			}
		}
		return json.Marshal(result)
	}
	if !validID(strings.TrimSpace(string(m["id"]))) {
		return nil, fmt.Errorf("上游未返回有效资源 ID")
	}
	return json.Marshal(pickFields(m, fields...))
}

func decimalID(id uint64) string { return strconv.FormatUint(id, 10) }
