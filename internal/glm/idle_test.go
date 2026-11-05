package glm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ctxBody 模拟 http 响应体：一旦请求 context 被 cancel，Read 立刻返回该 ctx 的错误。
// 真实 net/http body 就是这个行为，用它才能验证「空闲超时 → Read 报错」的路径。
type ctxBody struct{ ctx context.Context }

func (b *ctxBody) Read(p []byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b *ctxBody) Close() error               { return nil }

// TestIdleMonitorNormalizesTimeout 验证空闲监控超时后，Read 返回的是 ErrIdleTimeout
// 而不是裸 context.Canceled——否则上层会把「上游卡死」误判成「调用方取消」。
func TestIdleMonitorNormalizesTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rc := Monitor(&ctxBody{ctx: ctx}, 20*time.Millisecond, cancel)
	defer rc.Close()

	done := make(chan error, 1)
	go func() { _, err := rc.Read(make([]byte, 8)); done <- err }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrIdleTimeout) {
			t.Fatalf("err=%v, want ErrIdleTimeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read 未被空闲监控唤醒")
	}
}

// TestMonitorDisabled 验证 idle<=0 时原样返回底流（不做任何包装）。
func TestMonitorDisabled(t *testing.T) {
	rc := &ctxBody{ctx: context.Background()}
	if got := Monitor(rc, 0, func() {}); got != rc {
		t.Fatal("idle<=0 应原样返回底流")
	}
}
