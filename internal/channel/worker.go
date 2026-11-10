package channel

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"wild-work3/traework-ext/internal/protocol/chat"
	"wild-work3/traework-ext/internal/protocol/errkind"
	"wild-work3/traework-ext/internal/protocol/plugin"
	"wild-work3/traework-ext/internal/tw"
)

// generate 实现 chat.generate：把归一化请求改写为 SOLO 请求，流式回吐增量事件。
func (d *Daemon) generate(ctx context.Context, p Raw, emit plugin.Emit) (Raw, *Err) {
	var req chat.GenerateRequest
	if err := json.Unmarshal(p, &req); err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "请求体解析失败: %v", err)
	}
	cfg := parseConfig(p)
	// 会话面的渠道配置在请求体的 channel_config 里，必须合并（否则 variant 丢失）。
	cfg = mergeChannelConfig(cfg, req)

	upstreamModel, perr := stripVariantPrefix(req.Model, cfg)
	if perr != nil {
		return nil, perr
	}

	acct, c, err := d.account(req.AccountUID, cfg)
	if err != nil {
		k := errKindOf(err)
		return nil, errf(k, scopeOfKind(k), "%v", err)
	}

	body := upstreamBody(p, upstreamModel)
	if Log != nil {
		Log.Infof("chat.generate model=%s upstream=%s account=%s", req.Model, upstreamModel, authFileName(acct))
	}

	start := time.Now()
	rc, status, raw, err := c.ChatStream(acct, body)
	if err != nil {
		k := errKindOf(err)
		d.recordChat(acct, req.Model, nil, false, string(k), time.Since(start))
		return nil, errf(k, scopeOfKind(k), "%v", err)
	}
	if status >= 400 {
		k := tw.Classify(status, string(raw))
		if k == "" {
			k = errkind.Client
		}
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		d.recordChat(acct, req.Model, nil, false, string(k), time.Since(start))
		return nil, errf(k, scopeOfKind(k), "上游返回 %d: %s", status, msg)
	}
	defer rc.Close()

	var (
		firstMs  int64
		hadChunk bool
	)
	mark := func() {
		if firstMs == 0 {
			firstMs = time.Since(start).Milliseconds()
		}
	}
	res, streamErr := tw.ReadSOLOStream(rc, tw.StreamCallbacks{
		OnReasoning: func(s string) error {
			mark()
			hadChunk = true
			return emit("reasoning.delta", map[string]any{"content": s})
		},
		OnText: func(s string) error {
			mark()
			hadChunk = true
			return emit("text.delta", map[string]any{"content": s})
		},
		OnToolCall: func(tc tw.ToolCallDelta) error {
			mark()
			hadChunk = true
			return emit("tool_call.delta", map[string]any{
				"index":              tc.Index,
				"id":                 tc.ID,
				"type":               tc.Type,
				"name":               tc.Name,
				"arguments_fragment": tc.Arguments,
			})
		},
	})

	u := usageFromStream(res)
	u.TimeMs = time.Since(start).Milliseconds()
	u.FirstTokenMs = firstMs

	if streamErr != nil {
		if se, ok := tw.IsSOLOStreamError(streamErr); ok {
			k := errkind.Kind(se.Kind())
			d.recordChat(acct, req.Model, u, false, string(k), time.Since(start))
			return nil, errf(k, scopeOfKind(k), "%v", streamErr)
		}
		if ctx.Err() != nil {
			d.recordChat(acct, req.Model, u, false, string(errkind.Cancel), time.Since(start))
			return nil, errf(errkind.Cancel, errkind.ScopeRequest, "请求已取消")
		}
		// 流在正常结束前断开：按截断处理（已产生的增量已 emit）。
		k := errkind.Truncated
		if !hadChunk {
			k = errkind.Conn
		}
		d.recordChat(acct, req.Model, u, false, string(k), time.Since(start))
		return nil, errf(k, scopeOfKind(k), "响应流中断: %v", streamErr)
	}

	finish := "stop"
	if res != nil && res.FinishReason != "" {
		finish = res.FinishReason
	}
	if err := emit("finish_reason", map[string]any{"reason": finish}); err != nil {
		return nil, errf(errkind.Cancel, errkind.ScopeRequest, "写出事件失败: %v", err)
	}
	if err := emit("usage", *u); err != nil {
		return nil, errf(errkind.Cancel, errkind.ScopeRequest, "写出事件失败: %v", err)
	}
	if err := emit("completed", nil); err != nil {
		return nil, errf(errkind.Cancel, errkind.ScopeRequest, "写出事件失败: %v", err)
	}

	d.recordChat(acct, req.Model, u, true, "", time.Since(start))
	return mustJSON(u), nil
}

// stripVariantPrefix 校验并去掉模型上的变体前缀；跨变体前缀直接报 ModelBlocked。
func stripVariantPrefix(model string, cfg Config) (string, *Err) {
	m := strings.TrimSpace(model)
	if m == "" {
		return "", errf(errkind.ModelBlocked, errkind.ScopeModel, "模型名为空")
	}
	prof := cfg.profile()
	if strings.HasPrefix(m, prof.Prefix) {
		return strings.TrimPrefix(m, prof.Prefix), nil
	}
	for _, v := range []tw.Variant{tw.VariantWork, tw.VariantCode} {
		other := tw.ProfileFor(v)
		if other.Prefix == prof.Prefix {
			continue
		}
		if strings.HasPrefix(m, other.Prefix) {
			return "", errf(errkind.ModelBlocked, errkind.ScopeModel,
				"模型 %s 属于 %s 变体，当前渠道实例是 %s 变体，请把模型路由到对应渠道", model, other.Name, prof.Name)
		}
	}
	// 没有已知前缀时按裸模型名直接透传（便于 --raw 调试）。
	return m, nil
}

// upstreamBody 构造上游请求体：覆盖 model 为去前缀名，并剔除仅插件侧使用的字段。
func upstreamBody(p Raw, model string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(p, &obj); err != nil {
		return p
	}
	obj["model"] = model
	delete(obj, "channel_config")
	delete(obj, "account_uid")
	delete(obj, "extra")
	out, err := json.Marshal(obj)
	if err != nil {
		return p
	}
	return out
}

// usageFromStream 把 SOLO 流用量映射为归一化 chat.Usage。
func usageFromStream(res *tw.StreamResult) *chat.Usage {
	u := &chat.Usage{}
	if res == nil || res.Usage == nil {
		return u
	}
	nums, ok := tw.ExtractUsage(res.Usage)
	if !ok {
		return u
	}
	// 逐维标记来源（W3-34）：ExtractUsage 用 -1 表示「上游未报该字段」，
	// 只有真拿到值才算实报；本地推导出来的 total 不算。
	if nums.Prompt >= 0 {
		u.PromptTokens, u.PromptReported = nums.Prompt, true
	}
	if nums.Completion >= 0 {
		u.CompletionTokens, u.CompletionReported = nums.Completion, true
	}
	if nums.Total >= 0 {
		u.TotalTokens, u.TotalReported = nums.Total, true
	} else if u.PromptTokens > 0 || u.CompletionTokens > 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	if nums.CachedOK || nums.WriteOK {
		u.Cache = &chat.CacheUsage{
			Cached:         nums.Cached,
			CachedReported: nums.CachedOK,
			Write:          nums.Write,
			WriteReported:  nums.WriteOK,
		}
	}
	return u
}
