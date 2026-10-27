package worker

import (
	"agent-runtime/store"
	"context"
	"time"
)

// 本文件承载主动抢占取消（POST /tasks/{id}/cancel）的实现。
// 从 pool.go 同包正交拆出，只为守住「生产文件 ≤ 400 行」的架构门禁，
// 与 worker/session.go、worker/metrics.go 的拆分理由一致。

// userCancelCause 是用户主动取消的因果标记，用于与超时/停机取消精确区分。
type userCancelCause struct {
	reason string
}

func (c *userCancelCause) Error() string { return "user canceled: " + c.reason }

// Cancel 主动抢占指定在飞任务：通过 context 因果链物理中断正在进行的上游调用。
// 任务不在飞（未开始、已终态或不存在）时返回 ErrTaskNotRunning，由 API 层映射为 409。
func (p *Pool) Cancel(id, reason string) error {
	v, ok := p.inFlight.Load(id)
	if !ok {
		// 任务刚被抢占、句柄尚未登记完成：短暂让出 CPU 等一次，避免误判 409。
		for i := 0; i < 20; i++ {
			if tk, err := p.tasks.Get(id); err != nil || tk.Status != store.StatusRunning {
				break
			}
			time.Sleep(time.Millisecond)
			if v, ok = p.inFlight.Load(id); ok {
				break
			}
		}
	}
	cancel, _ := v.(context.CancelCauseFunc)
	if !ok || cancel == nil {
		return ErrTaskNotRunning
	}
	cancel(&userCancelCause{reason: reason})
	return nil
}
