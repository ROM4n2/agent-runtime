package main

import (
	"agent-runtime/llm"
	"agent-runtime/store"
	"agent-runtime/worker"
	"context"
	"net/http"
	"testing"
	"time"
)

// TestNewServerHasTimeouts 校验 newServer 工厂返回的服务器配置：
// 监听地址固定为 :8080，且三类超时均已设置为正值（避免慢客户端无限占用连接）。
func TestNewServerHasTimeouts(t *testing.T) {
	srv := newServer(nil)

	if srv.Addr != ":8080" {
		t.Errorf("Addr = %q, want %q", srv.Addr, ":8080")
	}
	if srv.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want > 0", srv.ReadHeaderTimeout)
	}
	if srv.WriteTimeout <= 0 {
		t.Errorf("WriteTimeout = %v, want > 0", srv.WriteTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v, want > 0", srv.IdleTimeout)
	}
}

func TestShutdown_Success(t *testing.T) {
	s := store.NewStore()
	p := worker.NewPool(1, time.Second, s, nil)
	p.Start()
	srv := &http.Server{}

	exitCode := shutdown(srv, p)
	if exitCode != 0 {
		t.Errorf("shutdown returned exitCode = %d, want 0", exitCode)
	}
}

type immediateChatter struct{}

func (immediateChatter) ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (*llm.AssistantTurn, error) {
	return &llm.AssistantTurn{Content: "recovered response"}, nil
}

// TestRecoverAndEnqueue_PendingTasks 模拟进程非正常死亡后的重启：
// pending 任务被重灌并执行完成，僵尸 running 被标定为 failed。
func TestRecoverAndEnqueue_PendingTasks(t *testing.T) {
	s := store.NewStore()
	pendingID := s.Create("resume me")
	zombieID := s.Create("was running")
	if err := s.Update(zombieID, store.StatusRunning); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	pendingIDs, recovered, err := s.RecoverTasks(context.Background())
	if err != nil {
		t.Fatalf("RecoverTasks: %v", err)
	}
	if recovered != 1 || len(pendingIDs) != 1 || pendingIDs[0] != pendingID {
		t.Fatalf("recovered=%d pending=%v, want 1/[%s]", recovered, pendingIDs, pendingID)
	}

	p := worker.NewPool(1, time.Second, s, immediateChatter{})
	p.Start()
	defer p.Stop()
	enqueueRecovered(context.Background(), p, pendingIDs)

	deadline := time.Now().Add(2 * time.Second)
	for {
		task, err := s.Get(pendingID)
		if err == nil && task.Status == store.StatusDone {
			if task.Result != "recovered response" {
				t.Errorf("result = %q, want %q", task.Result, "recovered response")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered pending task did not complete, last=%+v err=%v", task, err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	zombie, err := s.Get(zombieID)
	if err != nil {
		t.Fatalf("get zombie: %v", err)
	}
	if zombie.Status != store.StatusFailed || zombie.Error != store.ErrMsgServerRestart {
		t.Errorf("zombie = %+v, want failed with %q", zombie, store.ErrMsgServerRestart)
	}
}
