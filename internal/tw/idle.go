package tw

import (
	"context"
	"io"
	"sync"
	"time"
)

// DefaultIdleTimeout 是流式对话的默认空闲上限：上游超过该时长无任何字节即判定卡死。
const DefaultIdleTimeout = 180 * time.Second

// idleGuard 在 d 内无 Touch 时取消绑定 context，用于流式读取的空闲看门狗。
type idleGuard struct {
	mu     sync.Mutex
	timer  *time.Timer
	d      time.Duration
	cancel context.CancelFunc
	fired  bool
}

// withIdle 返回绑定 ctx 的派生 context 与看门狗。调用方必须在读完后 cancel。
func withIdle(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc, *idleGuard) {
	if d <= 0 {
		d = DefaultIdleTimeout
	}
	cctx, cancel := context.WithCancel(ctx)
	g := &idleGuard{d: d, cancel: cancel}
	g.timer = time.AfterFunc(d, func() {
		g.mu.Lock()
		g.fired = true
		g.mu.Unlock()
		cancel()
	})
	return cctx, cancel, g
}

// Touch 重置空闲计时（每读到数据时调用）。
func (g *idleGuard) Touch() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.fired {
		g.timer.Reset(g.d)
	}
}

// Stop 停止看门狗（读取正常结束）。
func (g *idleGuard) Stop() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.timer.Stop()
}

// TimedOut 报告看门狗是否已触发。
func (g *idleGuard) TimedOut() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fired
}

// idleReadCloser 包装响应体：每次有数据即 Touch，Close 时停止看门狗并 cancel。
type idleReadCloser struct {
	rc    io.ReadCloser
	guard *idleGuard
	stop  context.CancelFunc
}

func (r *idleReadCloser) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		r.guard.Touch()
	}
	return n, err
}

func (r *idleReadCloser) Close() error {
	r.guard.Stop()
	r.stop()
	return r.rc.Close()
}
