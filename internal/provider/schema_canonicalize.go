// schema_canonicalize.go 实现了 JSON Schema 的规范化，确保相同的逻辑 Schema
// 始终产生相同的字节表示。
//
// 主要用途:
//   - 稳定 prefix-cache 键: 规范化后的 Schema 序列化结果一致，提高缓存命中率
//   - 修复非标准 Schema: 某些 MCP 服务器发出 OpenAPI 风格的属性元数据
//     （如 {"required": true}），需要转换为 JSON Schema 的数组形式
//   - 处理空 Schema: 无参数工具返回空 Schema 时，填充合法的 {"type":"object"}
//     避免 json.Marshal 失败
//
// 规范化规则:
//   - 对 properties/$defs/definitions 等命名 Schema 映射的值递归规范化
//   - 对 required/dependentRequired 等数组字段排序（sortSchemaArray）
//   - 删除不合法的 required 字段（非数组形式）
package provider

import (
	"encoding/json"
	"sort"
)

// CanonicalizeSchema 递归规范化 JSON Schema，确保相同的逻辑 Schema 产生相同的字节表示。
// 空 Schema 填充为 {"type":"object"}，解析失败时原样返回。
func CanonicalizeSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		// A tool with no parameters (common for MCP tools) yields an empty
		// schema. An empty json.RawMessage makes json.Marshal of the enclosing
		// request fail ("unexpected end of JSON input") and bricks the whole
		// provider; emit a valid empty-object schema instead.
		return json.RawMessage(`{"type":"object"}`)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	canon := canonicalizeSchemaValue(v)
	b, err := json.Marshal(canon)
	if err != nil {
		return raw
	}
	return json.RawMessage(b)
}

func canonicalizeSchemaValue(v any) any {
	return canonicalizeSchemaObject(v)
}

func canonicalizeSchemaObject(v any) any {
	switch val := v.(type) {
	case map[string]any:
		for k, inner := range val {
			switch k {
			case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas":
				val[k] = canonicalizeNamedSchemas(inner)
			case "dependentRequired":
				val[k] = canonicalizeDependentRequired(inner)
			default:
				val[k] = canonicalizeSchemaObject(inner)
			}
		}
		if req, ok := val["required"]; ok {
			if arr, ok := req.([]any); ok {
				sortSchemaArray(arr)
			} else {
				// Some MCP servers emit OpenAPI-style property metadata such as
				// {"required": true}. OpenAI-compatible function schemas require
				// JSON Schema's array form; dropping the invalid value keeps the
				// whole tool list from being rejected with HTTP 400.
				delete(val, "required")
			}
		}
		if dr, ok := val["dependentRequired"]; ok && !isJSONObject(dr) {
			delete(val, "dependentRequired")
		}
		return val
	case []any:
		for i, elem := range val {
			val[i] = canonicalizeSchemaObject(elem)
		}
		return val
	default:
		return v
	}
}

// canonicalizeNamedSchemas 规范化命名 Schema 映射（如 properties、$defs）。
// 对映射中的每个 Schema 递归调用 canonicalizeSchemaObject。
func canonicalizeNamedSchemas(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return canonicalizeSchemaObject(v)
	}
	for name, schema := range m {
		m[name] = canonicalizeSchemaObject(schema)
	}
	return m
}

// canonicalizeDependentRequired 规范化 dependentRequired 字段。
// 确保每个值都是已排序的字符串数组，非法值被删除。
func canonicalizeDependentRequired(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	for key, inner := range m {
		if arr, ok := inner.([]any); ok {
			sortSchemaArray(arr)
		} else {
			delete(m, key)
		}
	}
	return m
}

func isJSONObject(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

// sortSchemaArray 对 Schema 数组进行稳定排序，按 JSON 序列化后的字符串比较。
// 排序确保 required 等数组字段的顺序一致，提高缓存命中率。
func sortSchemaArray(arr []any) {
	sort.SliceStable(arr, func(i, j int) bool {
		return schemaJSONString(arr[i]) < schemaJSONString(arr[j])
	})
}

func schemaJSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
