package tw

import (
	"encoding/json"
	"strconv"
	"strings"
)

// breakToolSchemaCycles 把所有本地 $ref 全量内联。
//
// Kimi/Moonshot 系后端对 tool schema 中任何 $ref 都返回 4027
// "detected infinite recursion without termination condition"（2026-10-13 对保留的
// 非环 $ref 亦然），因此不按模型名门控，统一内联。对容忍递归的后端，展开后的
// 有限 schema 依旧合法。
func breakToolSchemaCycles(obj map[string]any) {
	raw, present := obj["tools"]
	if !present {
		return
	}
	list, ok := raw.([]any)
	if !ok {
		return
	}
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		params, present := fn["parameters"]
		if !present {
			continue
		}
		switch m := params.(type) {
		case map[string]any:
			// 无 $ref 就不动原文：回写 Marshal 结果会重排键序、改写数字字面量，
			// 白白污染上游 prompt 缓存前缀。
			if countRefs(m) == 0 {
				continue
			}
			fn["parameters"] = coerceObject(inlineSchemaRefs(m, m, map[string]bool{}, newInlineBudget()))
		case string:
			var mm map[string]any
			if err := json.Unmarshal([]byte(m), &mm); err != nil {
				continue
			}
			if countRefs(mm) == 0 {
				continue
			}
			cleaned := coerceObject(inlineSchemaRefs(mm, mm, map[string]bool{}, newInlineBudget()))
			if s, err := json.Marshal(cleaned); err == nil {
				fn["parameters"] = string(s)
			}
		default:
			// 其他形态（数组/标量）保持原样。
		}
	}
}

// inlineBudget 限制单个 tool 的 $ref 内联展开次数，防止菱形引用组合爆炸。
const inlineBudget = 4096

func newInlineBudget() *int { b := inlineBudget; return &b }

// coerceObject 保证消毒结果为 object schema；非常规形态回落为空 schema。
func coerceObject(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// countRefs 递归统计节点中的 "$ref" 键数量。
func countRefs(node any) int {
	switch t := node.(type) {
	case map[string]any:
		n := 0
		for k, v := range t {
			if k == "$ref" {
				n++
			}
			n += countRefs(v)
		}
		return n
	case []any:
		n := 0
		for _, v := range t {
			n += countRefs(v)
		}
		return n
	}
	return 0
}

// inlineSchemaRefs 返回 node 的消毒副本：所有 $ref 就地内联（以目标内容为基底
// 继续内联，$ref 的兄弟键按 draft-07 语义丢弃）。chain 记录当前展开链上已入栈
// 的 $ref 目标指针（分叉时复制），链上重现即递归环，截断为空 schema 终止。
// root 为整个 parameters 文档（$ref 解析基准）。budget 防组合爆炸。不改写入参。
func inlineSchemaRefs(node, root any, chain map[string]bool, budget *int) any {
	switch t := node.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok {
			if ptr, isLocal := localRefPtr(ref); isLocal && ptr != "" && !chain[ptr] {
				if target := resolveLocalRef(root, ptr); target != nil && *budget > 0 {
					*budget--
					child := make(map[string]bool, len(chain)+1)
					for k := range chain {
						child[k] = true
					}
					child[ptr] = true
					return inlineSchemaRefs(target, root, child, budget)
				}
			}
			return map[string]any{} // "#" 必环 / 成环 / 非本地 / 不可达 / 预算耗尽
		}
		out := make(map[string]any, len(t))
		for k, v := range t {
			out[k] = inlineSchemaRefs(v, root, chain, budget)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			out[i] = inlineSchemaRefs(v, root, chain, budget)
		}
		return out
	}
	return node
}

// localRefPtr 解析本地 JSON Pointer 引用；返回 (指针, 是否本地)。
// "#" 解析为空指针并视为本地——它必然指向根，属必环，由 chain 截断。
func localRefPtr(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, "#") {
		return "", false
	}
	ptr := strings.TrimPrefix(ref, "#")
	if ptr == "" {
		return "", true
	}
	if !strings.HasPrefix(ptr, "/") {
		return "", false
	}
	return ptr, true
}

// resolveLocalRef 按 RFC6901 解析本地指针（~1→/，~0→~）；不可达返回 nil。
func resolveLocalRef(root any, ptr string) any {
	cur := root
	for _, part := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		part = strings.ReplaceAll(part, "~1", "/")
		part = strings.ReplaceAll(part, "~0", "~")
		switch t := cur.(type) {
		case map[string]any:
			v, ok := t[part]
			if !ok {
				return nil
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(part)
			if err != nil || idx < 0 || idx >= len(t) {
				return nil
			}
			cur = t[idx]
		default:
			return nil
		}
	}
	return cur
}
