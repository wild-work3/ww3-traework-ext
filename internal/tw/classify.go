package tw

import (
	"strings"

	"wild-work3/traework-ext/internal/protocol/errkind"
)

// Error 是上游 HTTP 错误的归一化载体：Kind 已按状态码优先规则映射。
type Error struct {
	Kind   errkind.Kind
	Status int
	Msg    string
}

// Error 实现 error。
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Status > 0 {
		return "traework upstream status=" + itoa(e.Status) + ": " + e.Msg
	}
	return e.Msg
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// Classify 按**状态码优先**规则把上游响应映射为 errkind.Kind。
//
// 顺序是关键：429/401 的响应体里也可能带业务码 1005，若先看 1005 就会把限流/会话
// 失效误判成硬额度耗尽（触发 12h 冷却或跳过 401 自愈重登）。因此先看状态码。
func Classify(status int, body string) errkind.Kind {
	switch {
	case status == 401:
		return errkind.SessionDead
	case status == 429:
		return errkind.SoftRate
	case bodyHasCredit1005(body):
		return errkind.HardCredit
	case status == 404:
		return errkind.NotFound
	case status >= 500:
		return errkind.Server
	case status >= 400:
		return errkind.Client
	}
	return ""
}

// bodyHasCredit1005 判断响应体是否声明业务错误码 1005（额度耗尽）。
func bodyHasCredit1005(body string) bool {
	if body == "" {
		return false
	}
	if strings.Contains(body, `"code":1005`) || strings.Contains(body, `"code": 1005`) {
		return true
	}
	l := strings.ToLower(body)
	return strings.Contains(l, "1005") && strings.Contains(l, "plan")
}

// errorFor 由 HTTP 状态与响应体构造 *Error。
func errorFor(status int, body string) *Error {
	return &Error{Kind: Classify(status, body), Status: status, Msg: truncate(body, 300)}
}

// truncate 截断过长文本（日志/错误用）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
