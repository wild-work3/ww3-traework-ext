package tw

import (
	"strings"
	"sync"
	"time"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic/JWT 读，防止并发读写 token。
	mu sync.RWMutex

	Kind         string // 固定 "traework"
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64  // Unix 秒
	Domain       string // trae.cn
	ApiHost      string // https://api.trae.com.cn
	MachineID    string // x-machine-id
	DeviceID     string // x-device-id（小写头）
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处
}

// Lock 供同进程内其他代码在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// RLock 供读路径持有读锁。
func (a *Auth) RLock() { a.mu.RLock() }

// RUnlock 释放读锁。
func (a *Auth) RUnlock() { a.mu.RUnlock() }

// JWT 返回当前 access token 快照（TraeWork 头也叫 Cloud-IDE-JWT）。
func (a *Auth) JWT() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// AccessTokenValue 锁内快照：出站请求头用。
func (a *Auth) AccessTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// RefreshTokenValue 锁内快照。
func (a *Auth) RefreshTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshToken
}

// NeedsRefreshLocked 是 NeedsRefresh 的持锁内部版本。
func (a *Auth) NeedsRefreshLocked(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// NeedsRefresh 报告 token 是否将在 within 内过期。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.NeedsRefreshLocked(within)
}

// Region 返回 "cn" 或 "global"。domain 为空视为 CN。
func (a *Auth) Region() string {
	d := strings.ToLower(strings.TrimSpace(a.Domain))
	if d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") {
		return "global"
	}
	return "cn"
}
