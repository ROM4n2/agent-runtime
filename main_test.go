package main

import (
	"agent-runtime/config"
	"agent-runtime/llm"
	"agent-runtime/store"
	"agent-runtime/worker"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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

// TestRunIndexCommand_BuildsAndExits 验证 -index 子命令装配（ADR-0017 决策 6）：
// 3 篇语料 → buildIndex 真实重建（报告数字）→ runIndexCmd 幂等重跑仍退出 0 →
// 独立连接确认 doc_chunk 物理落库（索引与任务表解耦，db_path 即索引库文件）。
// conf 经 env（DB_PATH）指向临时库：不碰仓库真实的 config.yaml 与 db 文件。
func TestRunIndexCommand_BuildsAndExits(t *testing.T) {
	corpus := t.TempDir()
	for i := 1; i <= 3; i++ {
		writeCorpusDoc(t, corpus, fmt.Sprintf("doc%d.md", i),
			fmt.Sprintf("检索系统设计与实现 第 %d 篇：分块事务、内容指纹与混合召回通道。", i))
	}
	dbPath := filepath.Join(t.TempDir(), "index.db")
	t.Setenv("DB_PATH", dbPath)
	conf, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if conf.DbPath != dbPath {
		t.Fatalf("conf.DbPath = %q, want %q (env must override config.yaml)", conf.DbPath, dbPath)
	}

	// 第一次构建：真实重建，报告数字必须反映 3 篇语料（slog 结构化摘要的同一数据源）。
	report, err := buildIndex(conf, corpus)
	if err != nil {
		t.Fatalf("buildIndex: %v", err)
	}
	if report.DocsIndexed != 3 {
		t.Errorf("DocsIndexed = %d, want 3", report.DocsIndexed)
	}
	if report.ChunksWritten == 0 {
		t.Error("ChunksWritten = 0, want > 0 (3 docs must produce chunks)")
	}
	if report.Fingerprint == "" {
		t.Error("Fingerprint = \"\", want the content-hash digest")
	}

	// 子命令入口：同库同语料再跑 → 指纹命中幂等跳过，仍退出 0。
	if code := runIndexCmd(conf, corpus); code != 0 {
		t.Fatalf("runIndexCmd = %d, want 0", code)
	}

	// 物理落库验证（不走 Index 内部句柄）：buildIndex 已关闭其连接，
	// doc_chunk 行必须仍在盘上——证明索引落在 db_path 文件而非一次性内存。
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open inspect connection: %v", err)
	}
	defer db.Close()
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM doc_chunk").Scan(&rows); err != nil {
		t.Fatalf("count doc_chunk: %v", err)
	}
	if rows == 0 {
		t.Error("doc_chunk row count = 0, want > 0 after buildIndex")
	}
}

// TestRunIndexCommand_MissingDir 验证 -index 对不存在语料目录的显式失败：
// Rebuild 的 os.Stat 错误必须上浮为非 0 退出码。
// 变异自证 ④：把失败退出码改成 0，本断言转红。
func TestRunIndexCommand_MissingDir(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	t.Setenv("DB_PATH", dbPath)
	conf, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	if code := runIndexCmd(conf, missing); code == 0 {
		t.Errorf("runIndexCmd(missing dir) = 0, want non-zero (explicit failure, no silent success)")
	}
}

// TestNewEmbedder_RequiresAllThreeCredentials 钉死 ADR-0017 决策 3 的「默认关闭」：
// EmbedBaseURL/EmbedAPIKey/EmbedModel 三者全非空才构造 embedder，缺任一即 nil
// （纯词法模式，禁静默半配）。变异自证 ③：把「三者全非空」条件改成任一非空即转红。
func TestNewEmbedder_RequiresAllThreeCredentials(t *testing.T) {
	if newEmbedder(config.Config{
		EmbedBaseURL: "https://emb.example.com", EmbedAPIKey: "k", EmbedModel: "m", EmbedDim: 8,
	}) == nil {
		t.Error("newEmbedder(all three credentials) = nil, want a Client implementing llm.Embedder")
	}
	partials := []struct {
		name string
		cfg  config.Config
	}{
		{"none set", config.Config{}},
		{"base url only", config.Config{EmbedBaseURL: "https://emb.example.com"}},
		{"api key only", config.Config{EmbedAPIKey: "k"}},
		{"model only", config.Config{EmbedModel: "m"}},
		{"url+key, model missing", config.Config{EmbedBaseURL: "https://emb.example.com", EmbedAPIKey: "k"}},
	}
	for _, tc := range partials {
		t.Run(tc.name, func(t *testing.T) {
			if got := newEmbedder(tc.cfg); got != nil {
				t.Errorf("newEmbedder(%s) = %v, want nil (partial embed config must stay lexical)", tc.name, got)
			}
		})
	}
}

// writeCorpusDoc 在临时语料目录写一篇 .md 文档（供 -index 测试造 3 篇语料）。
func writeCorpusDoc(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
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
