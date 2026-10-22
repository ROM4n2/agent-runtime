package agent

import (
	"testing"
	"time"
)

func TestTraceBuffer_SubscribeSnapshotAndEvents(t *testing.T) {
	tb := NewTraceBuffer(8)
	tb.Append(TraceStep{Step: 0, Kind: KindThink, Detail: "before"})

	snapshot, events, cancel := tb.Subscribe()
	if len(snapshot) != 1 || snapshot[0].Detail != "before" {
		t.Fatalf("snapshot = %+v, want one step %q", snapshot, "before")
	}

	tb.Append(TraceStep{Step: 1, Kind: KindFinal, Detail: "after"})

	select {
	case got := <-events:
		if got.Detail != "after" {
			t.Errorf("event detail = %q, want %q", got.Detail, "after")
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive appended step")
	}

	cancel()
	if _, ok := <-events; ok {
		t.Error("events channel should be closed after cancel")
	}
	cancel() // 幂等：重复 cancel 不得 panic
}

func TestTraceBuffer_MultipleSubscribers(t *testing.T) {
	tb := NewTraceBuffer(8)
	_, ch1, cancel1 := tb.Subscribe()
	defer cancel1()
	_, ch2, cancel2 := tb.Subscribe()
	defer cancel2()

	tb.Append(TraceStep{Step: 0, Kind: KindThink, Detail: "broadcast"})

	for i, ch := range []<-chan TraceStep{ch1, ch2} {
		select {
		case got := <-ch:
			if got.Detail != "broadcast" {
				t.Errorf("subscriber %d got %q", i, got.Detail)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d did not receive broadcast", i)
		}
	}
}

// TestTraceBuffer_SlowSubscriberDoesNotBlockAppend 验证慢消费者只丢自己的事件，
// 绝不阻塞任务执行路径（Append 是非阻塞广播）。
func TestTraceBuffer_SlowSubscriberDoesNotBlockAppend(t *testing.T) {
	tb := NewTraceBuffer(4)
	_, _, cancel := tb.Subscribe() // 只订阅，永不读取
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			tb.Append(TraceStep{Step: i, Kind: KindThink})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Append blocked on a slow subscriber")
	}

	if got := len(tb.Snapshot()); got != 4 {
		t.Errorf("snapshot len = %d, want bounded capacity 4", got)
	}
}
