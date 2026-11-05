// idle.go 聊天 SSE 流的「流中空闲监控」：读到数据即续命，静默超阈值主动断流。
//
// 背景：为修复「长回答被总超时截断」，Stream 用无总时长的 http.Client。这会移除原有的
// 总超时兜底——若上游连上并在发出首字节后中途卡死不再发数据，请求会无限期挂起。
// 本文件在返回给调用方的 body 外层包一层监控：每次读到数据就刷新时间戳，后台按周期
// 检查，静默超过 idle 即 cancel 请求 context，使阻塞中的 Read 立即返回错误。
//
// 首字节阶段不由本文件负责：Transport 的 ResponseHeaderTimeout 仍约束「响应头到达前」。
// （移植自 glm-ext/internal/glm/idle.go，语义原样保留。）
package glm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// DefaultTimeout 生产默认的流中空闲阈值：静默超过该时长即判定上游卡死并断流。
const DefaultTimeout = 300 * time.Second

// ErrIdleTimeout 流中空闲超时（上游连上后卡死不发送数据）。
var ErrIdleTimeout = errors.New("glm: 上游流式响应空闲超时")

// monitoringBody 包在流式 body 外层：每次读到底层数据（n>0）就刷新 lastRead；
// 后台 goroutine 周期检查，静默超过 idle 就 cancel 请求 context，中断阻塞中的 Read。
type monitoringBody struct {
	rc        io.ReadCloser
	cancel    context.CancelFunc
	idle      time.Duration
	mu        sync.Mutex
	lastRead  time.Time
	closed    bool
	idleFired bool // 已因空闲超时主动断流，供 Read 归一化错误
	done      chan struct{}
}

// Read 透传读取；读到底层数据（n>0）时刷新 lastRead 续命。
// 若本次读错误源于空闲超时主动 cancel，则归一化成 ErrIdleTimeout，让上层能区分
// 「上游卡死」与「调用方取消」（两者错误文本都是 context canceled）。
func (b *monitoringBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.mu.Lock()
		b.lastRead = time.Now()
		b.mu.Unlock()
	}
	if err != nil && b.fired() {
		return n, ErrIdleTimeout
	}
	return n, err
}

func (b *monitoringBody) fired() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.idleFired
}

// Close 幂等：停后台监控、取消请求 context、关闭底流，保证无泄漏。
func (b *monitoringBody) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	close(b.done)
	b.mu.Unlock()
	b.cancel()
	return b.rc.Close()
}

func (b *monitoringBody) idleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.lastRead)
}

// Monitor 若 idle<=0 直接返回原底流（禁用空闲监控）；否则包上流中空闲监控。
//
// cancel 的所有权随返回值转移：idle>0 时由监控 body 的 Close 负责调用（含空闲超时主动调用）。
func Monitor(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if idle <= 0 {
		return rc
	}
	b := &monitoringBody{rc: rc, cancel: cancel, idle: idle, lastRead: time.Now(), done: make(chan struct{})}
	go func() {
		t := time.NewTicker(tick(idle))
		defer t.Stop()
		for {
			select {
			case <-b.done:
				return
			case <-t.C:
				if b.idleFor() > b.idle {
					b.mu.Lock()
					b.idleFired = true
					b.mu.Unlock()
					b.cancel()
					return
				}
			}
		}
	}()
	return b
}

// WithCancel 给请求挂上可取消的 context，返回新请求与 cancel（cancel 交给 Monitor 接管）。
func WithCancel(req *http.Request) (*http.Request, context.CancelFunc) {
	ctx, cancel := context.WithCancel(req.Context())
	return req.WithContext(ctx), cancel
}

// tick 返回监控周期：idle/4，钳在 [10ms, 1s]。
func tick(idle time.Duration) time.Duration {
	d := idle / 4
	if d > time.Second {
		d = time.Second
	}
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}
