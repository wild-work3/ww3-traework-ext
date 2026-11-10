package channel

import (
	"wild-work3/traework-ext/internal/protocol/errkind"
	"wild-work3/traework-ext/internal/protocol/jsonl"
)

// Raw 是协议裸 JSON。
type Raw = jsonl.RawMessage

// Err 是协议错误对象。
type Err = jsonl.ErrObj

// errf 构造协议错误。
func errf(kind errkind.Kind, scope errkind.Scope, format string, a ...any) *Err {
	return jsonl.Errorf(kind, scope, format, a...)
}
