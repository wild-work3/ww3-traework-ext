package tw

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// SOLOEvent 是 SOLO 命名事件流的一条事件（对齐旧版 solosse.go）。
type SOLOEvent struct {
	Event        string          `json:"-"`
	Response     string          `json:"response"`
	Reasoning    string          `json:"reasoning_content"`
	ToolCalls    json.RawMessage `json:"tool_calls"`
	Usage        map[string]any  `json:"-"`
	FinishReason string          `json:"finish_reason"`
	ErrorCode    int64           `json:"code"`
	ErrorMessage string          `json:"message"`
	QueuePos     int64           `json:"position"`
}

// SOLOStreamError 是 SOLO 流内 error 事件的类型化错误。
type SOLOStreamError struct {
	Code int64
	Msg  string
}

func (e *SOLOStreamError) Error() string {
	return fmt.Sprintf("solo error code=%d msg=%s", e.Code, e.Msg)
}

// Kind 把 SOLO 业务错误码映射到 errkind。
func (e *SOLOStreamError) Kind() string {
	switch e.Code {
	case 1005:
		return "hard_credit"
	case 3004, 9074:
		return "soft_rate"
	default:
		return "client"
	}
}

// ParseSOLOLine 把一条 (event, data) 解析为 SOLOEvent；非 JSON 或空数据返回 false。
func ParseSOLOLine(eventName, dataLine string) (SOLOEvent, bool) {
	data := strings.TrimSpace(dataLine)
	if data == "" {
		return SOLOEvent{}, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return SOLOEvent{}, false
	}
	ev := SOLOEvent{Event: eventName}
	if v, ok := raw["response"]; ok {
		_ = json.Unmarshal(v, &ev.Response)
	}
	if v, ok := raw["reasoning_content"]; ok {
		_ = json.Unmarshal(v, &ev.Reasoning)
	}
	if v, ok := raw["tool_calls"]; ok {
		ev.ToolCalls = append(json.RawMessage(nil), v...)
	}
	if v, ok := raw["finish_reason"]; ok {
		_ = json.Unmarshal(v, &ev.FinishReason)
	}
	if v, ok := raw["code"]; ok {
		_ = json.Unmarshal(v, &ev.ErrorCode)
	}
	if v, ok := raw["message"]; ok {
		_ = json.Unmarshal(v, &ev.ErrorMessage)
	}
	if v, ok := raw["position"]; ok {
		_ = json.Unmarshal(v, &ev.QueuePos)
	}
	if eventName == "token_usage" {
		var m map[string]any
		if json.Unmarshal([]byte(data), &m) == nil {
			ev.Usage = m
		}
	}
	return ev, true
}

// soloToolCall 是流式工具调用的一帧。
type soloToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Function *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	FunctionCall *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function_call"`
}

// ToolCallDelta 是解析后的一帧工具调用增量。
type ToolCallDelta struct {
	Index     int
	ID        string
	Type      string
	Name      string
	Arguments string
}

// ParseToolCallDeltas 解析 output 事件里的 tool_calls 数组。
func ParseToolCallDeltas(raw json.RawMessage) []ToolCallDelta {
	if len(raw) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var arr []soloToolCall
	if strings.HasPrefix(trimmed, "[") {
		if json.Unmarshal(raw, &arr) != nil {
			return nil
		}
	} else if strings.HasPrefix(trimmed, "{") {
		var one soloToolCall
		if json.Unmarshal(raw, &one) != nil {
			return nil
		}
		arr = []soloToolCall{one}
	}
	out := make([]ToolCallDelta, 0, len(arr))
	for _, tc := range arr {
		d := ToolCallDelta{Index: tc.Index, ID: tc.ID, Type: tc.Type, Name: tc.Name}
		if tc.Function != nil {
			if d.Name == "" {
				d.Name = tc.Function.Name
			}
			d.Arguments = tc.Function.Arguments
		}
		if tc.FunctionCall != nil {
			if d.Name == "" {
				d.Name = tc.FunctionCall.Name
			}
			d.Arguments = tc.FunctionCall.Arguments
		}
		if d.Type == "" {
			d.Type = "function"
		}
		out = append(out, d)
	}
	return out
}

// StreamCallbacks 是流式解析的回调集合，任一可为 nil。
type StreamCallbacks struct {
	OnText      func(string) error
	OnReasoning func(string) error
	OnToolCall  func(ToolCallDelta) error
	OnQueue     func(int64)
}

// StreamResult 汇总一次流式解析的结果。
type StreamResult struct {
	Usage        map[string]any
	FinishReason string
}

// ReadSOLOStream 顺序读取 SOLO 命名事件流并回调。
// 流内 error 事件返回 *SOLOStreamError；读到 EOF 未出错则正常结束。
func ReadSOLOStream(r io.Reader, cb StreamCallbacks) (*StreamResult, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var event string
	var data strings.Builder
	res := &StreamResult{}

	flush := func() error {
		if event == "" && data.Len() == 0 {
			return nil
		}
		ev, ok := ParseSOLOLine(event, data.String())
		event = ""
		data.Reset()
		if !ok {
			return nil
		}
		switch ev.Event {
		case "output":
			if ev.Reasoning != "" && cb.OnReasoning != nil {
				if err := cb.OnReasoning(ev.Reasoning); err != nil {
					return err
				}
			}
			if ev.Response != "" && cb.OnText != nil {
				if err := cb.OnText(ev.Response); err != nil {
					return err
				}
			}
			for _, d := range ParseToolCallDeltas(ev.ToolCalls) {
				if cb.OnToolCall != nil {
					if err := cb.OnToolCall(d); err != nil {
						return err
					}
				}
			}
		case "token_usage":
			res.Usage = NormalizeUsageShape(ev.Usage)
		case "done":
			if ev.FinishReason != "" {
				res.FinishReason = ev.FinishReason
			}
		case "error":
			return &SOLOStreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
		case "queue_begin", "request_wait_in_queue":
			if cb.OnQueue != nil {
				cb.OnQueue(ev.QueuePos)
			}
		}
		return nil
	}

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if err := flush(); err != nil {
				return res, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
			continue
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	if err := flush(); err != nil {
		return res, err
	}
	if res.FinishReason == "" {
		res.FinishReason = "stop"
	}
	return res, nil
}

// IsSOLOStreamError 判定错误是否为流内业务错误。
func IsSOLOStreamError(err error) (*SOLOStreamError, bool) {
	var se *SOLOStreamError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}
