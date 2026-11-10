package tw

import (
	"encoding/json"
	"strings"
)

// PrepareBody 把 OpenAI chat/completions 请求改写为 Trae SOLO llm_utils_chat 请求。
//
// function 区分办公版（solo_work_lite）与代码版（solo_agent）。模型目录会登记
// 每个模型实际所在的池（见 modelpool.go），此处据此覆盖 function——用错池的
// 请求上游返回 4001 param is invalid。
func PrepareBody(src []byte, function string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	// 缺省补 include_usage：否则末帧无 usage（旧版 D7；客户端显式给了就不覆盖）。
	if _, ok := obj["stream_options"]; !ok {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	if msgs, ok := obj["messages"].([]any); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			content, present := m["content"]
			role, _ := m["role"].(string)
			if role == "assistant" {
				if tcs, ok := m["tool_calls"].([]any); ok {
					kept := make([]any, 0, len(tcs))
					for _, tci := range tcs {
						tc, ok := tci.(map[string]any)
						if !ok {
							continue
						}
						if fn, ok := tc["function"].(map[string]any); ok {
							tc["function_call"] = fn
							delete(tc, "function")
						}
						if fc, ok := tc["function_call"].(map[string]any); ok {
							name, _ := fc["name"].(string)
							if strings.TrimSpace(name) == "" {
								continue
							}
						}
						kept = append(kept, tc)
					}
					if len(kept) == 0 {
						delete(m, "tool_calls")
					} else {
						m["tool_calls"] = kept
					}
				}
			}
			if !present || content == nil {
				continue
			}
			if s, ok := content.(string); ok {
				m["content"] = []any{map[string]any{"type": "text", "text": s}}
			}
		}
	}
	model, _ := obj["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultConfig
	}
	obj["config_name"] = model
	obj["model"] = model
	// 按模型实际归属池覆盖 function；未登记的模型沿用渠道默认。
	if fn := FunctionForModel(model); fn != "" {
		function = fn
	}
	obj["function"] = function
	normalizeToolChoice(obj)
	// Kimi/Moonshot 系后端拒绝 tool schema 中的 $ref（4027），须在 normalizeTools
	// 把 parameters 字符串化之前全量内联。不按模型名门控。
	breakToolSchemaCycles(obj)
	normalizeTools(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeToolChoice 只做规范化 function 选择到 SOLO wire 名称的结构投影。
func normalizeToolChoice(obj map[string]any) {
	tc, ok := obj["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "function" {
		return
	}
	fn, ok := tc["function"].(map[string]any)
	if !ok {
		return
	}
	name, _ := fn["name"].(string)
	if name != "" {
		obj["tool_choice"] = name
	}
}

// normalizeTools 把每个工具 parameters 对象序列化为字符串（上游要求）。
func normalizeTools(obj map[string]any) {
	raw, present := obj["tools"]
	if !present {
		return
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"]; ok {
			if paramsMap, isMap := params.(map[string]any); isMap {
				if s, err := json.Marshal(paramsMap); err == nil {
					fn["parameters"] = string(s)
				}
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = out
}
