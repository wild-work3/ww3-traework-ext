package tw

import "encoding/json"

// NormalizeUsageShape 把 Anthropic 命名的缓存 token 归一到 OpenAI 形：
// cache_read_input_tokens / cache_creation_input_tokens → prompt_tokens_details.cached_tokens。
// 就地修改并返回同一 map（旧版 issue #104：缓存命中恒为 0）。
func NormalizeUsageShape(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	read := numField(usage, "cache_read_input_tokens")
	write := numField(usage, "cache_creation_input_tokens")
	if read < 0 && write < 0 {
		return usage
	}
	details, _ := usage["prompt_tokens_details"].(map[string]any)
	if details == nil {
		details = map[string]any{}
	}
	if read >= 0 {
		details["cached_tokens"] = float64(read)
	}
	if write >= 0 {
		details["cache_creation_tokens"] = float64(write)
	}
	usage["prompt_tokens_details"] = details
	return usage
}

// numField 读取数字字段，缺失返回 -1。
func numField(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok {
		return -1
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
	}
	return -1
}

// UsageNumbers 是从上游 usage map 中提取出的用量事实。
type UsageNumbers struct {
	Prompt     int
	Completion int
	Total      int
	Cached     int
	CachedOK   bool
	Write      int
	WriteOK    bool
}

// ExtractUsage 从归一化后的 usage map 提取数值（缺失字段保持 -1 语义由调用方处理）。
func ExtractUsage(usage map[string]any) (UsageNumbers, bool) {
	if usage == nil {
		return UsageNumbers{}, false
	}
	var u UsageNumbers
	u.Prompt = int(firstNonNeg(usage, "prompt_tokens", "input_tokens"))
	u.Completion = int(firstNonNeg(usage, "completion_tokens", "output_tokens"))
	u.Total = int(firstNonNeg(usage, "total_tokens"))
	if u.Total < 0 {
		if u.Prompt >= 0 && u.Completion >= 0 {
			u.Total = u.Prompt + u.Completion
		}
	}
	if d, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if c := numField(d, "cached_tokens"); c >= 0 {
			u.Cached = int(c)
			u.CachedOK = true
		}
		if w := numField(d, "cache_creation_tokens"); w >= 0 {
			u.Write = int(w)
			u.WriteOK = true
		}
	}
	return u, true
}

// firstNonNeg 返回首个 >=0 的数字字段，否则 -1。
func firstNonNeg(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v := numField(m, k); v >= 0 {
			return v
		}
	}
	return -1
}
