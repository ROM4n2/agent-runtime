package main

import (
	"agent-runtime/config"
	"agent-runtime/llm"
	"agent-runtime/store"
	"agent-runtime/worker"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// TestRunWithArgs_FlagErrors 钉死用法错误的退出码口径（P5 黄卡 B①/B③）：
// 未知 flag 与 -index 缺值都属于「用法错误」→ 2，与运行期 harness 故障（1）
// 严格区分，脚本据此能判断「命令写错了」还是「跑挂了」。
// 变异自证：把 -index 缺值分支的 return 2 改成 0，本用例转红。
func TestRunWithArgs_FlagErrors(t *testing.T) {
	withDefaultLogger(t)
	if code := runWithArgs([]string{"-no-such-flag"}); code != 2 {
		t.Errorf("runWithArgs(unknown flag) = %d, want 2", code)
	}
	// 完全缺值：flag 包解析即失败（flag needs an argument），不进任何重装配。
	if code := runWithArgs([]string{"-index"}); code != 2 {
		t.Errorf("runWithArgs(-index without value) = %d, want 2", code)
	}
	t.Run("explicit empty index dir", func(t *testing.T) {
		// 隔离 cwd：config.Load("") 读的是相对路径 config.yaml，仓库里那份不该参与。
		t.Chdir(t.TempDir())
		if code := runWithArgs([]string{"-index", ""}); code != 2 {
			t.Errorf("runWithArgs(-index \"\") = %d, want 2 (usage hint, no silent build)", code)
		}
	})
}

// TestRunWithArgs_HelpExitsZero 钉死 -h/-help 的退出码口径（YELLOW-2）：
// 帮助是用户的显式请求而非用法错误 → 0，与未知 flag 的 2 严格区分；
// 同时必须短路启动流程，不能继续 config.Load / 校验密钥 / 起服务。
// 变异自证：把 flag.ErrHelp 分支改回 return 2，本用例的 -h 档转红。
func TestRunWithArgs_HelpExitsZero(t *testing.T) {
	withDefaultLogger(t)
	for _, arg := range []string{"-h", "-help", "--help"} {
		t.Run(arg, func(t *testing.T) {
			t.Chdir(t.TempDir()) // 万一漏了短路，也不会读到仓库里的 config.yaml
			if code := runWithArgs([]string{arg}); code != 0 {
				t.Errorf("runWithArgs(%s) = %d, want 0 (help is a request, not a usage error)", arg, code)
			}
		})
	}
	if code := runWithArgs([]string{"-no-such-flag"}); code != 2 {
		t.Errorf("runWithArgs(unknown flag) = %d, want 2 (unknown flag stays a usage error)", code)
	}
}

// TestRunWithArgs_IndexShortCircuitsAPIKeyCheck 验证 -index 早退出的顺序（P5 黄卡 B②）：
// 建索引不依赖 LLM，因此无论 DEEPSEEK_API_KEY 有没有，都必须走构建路径并退出 0；
// 若密钥校验被挪到子命令之前，缺密钥那一档会变成 1（本用例转红）。
// 落库断言（doc_chunk 有行）证明走的是真构建路径，不是空转过早退出。
func TestRunWithArgs_IndexShortCircuitsAPIKeyCheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		apiKey string
	}{
		{"without DEEPSEEK_API_KEY", ""},
		{"with DEEPSEEK_API_KEY", "sk-fake-for-index-build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withDefaultLogger(t) // runWithArgs 会改写默认 logger，用例结束必须还原
			t.Chdir(t.TempDir()) // 隔离 config.yaml：密钥只可能来自环境变量
			t.Setenv("DEEPSEEK_API_KEY", tc.apiKey)

			corpus := t.TempDir()
			writeCorpusDoc(t, corpus, "d1.md", "分块事务、内容指纹与混合召回通道。")
			dbPath := filepath.Join(t.TempDir(), "index.db")
			t.Setenv("DB_PATH", dbPath) // 落盘而非 :memory:，便于事后核对

			if code := runWithArgs([]string{"-index", corpus}); code != 0 {
				t.Fatalf("runWithArgs(-index) = %d, want 0 (-index must short-circuit before the API-key check)", code)
			}

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
				t.Error("doc_chunk row count = 0, want > 0 (the build path must actually run)")
			}
		})
	}
}

// TestRunIndexCommand_IdempotentRebuild 深度断言幂等语义（P5 黄卡 C）：
// 同一库同一语料第二次构建时，retrieval.Rebuild 走「指纹相同 → 幂等跳过」分支，
// 必须返回 DocsIndexed=0 / ChunksWritten=0 且指纹不变——这才证明第二次是「没重建」，
// 而不是「重建了一遍数字恰好相同」。runIndexCmd 只返回退出码，报告数字从同一
// buildIndex 取（子命令只是它的日志外壳，这里顺带跑一次确认退出码仍为 0）。
// 变异自证：让 Rebuild 忽略指纹每次都重建，DocsIndexed=0 与 ChunksWritten=0 断言转红。
// cwd 隔离同批其他用例：不隔离时 config.Load("") 会读开发机仓库的 config.yaml，
// 一旦里面有 embed_* 凭证就会构造真 embedder 并发网络请求（测试不该依赖网络）。
func TestRunIndexCommand_IdempotentRebuild(t *testing.T) {
	t.Chdir(t.TempDir())
	corpus := t.TempDir()
	for i := 1; i <= 2; i++ {
		writeCorpusDoc(t, corpus, fmt.Sprintf("doc%d.md", i),
			fmt.Sprintf("第 %d 篇：幂等重建必须按内容指纹整体跳过，不重复写 chunk。", i))
	}
	dbPath := filepath.Join(t.TempDir(), "index.db")
	t.Setenv("DB_PATH", dbPath)
	conf, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if conf.DbPath != dbPath {
		t.Fatalf("conf.DbPath = %q, want %q (temp db, never the repo one)", conf.DbPath, dbPath)
	}

	first, err := buildIndex(conf, corpus)
	if err != nil {
		t.Fatalf("first buildIndex: %v", err)
	}
	if first.DocsIndexed == 0 || first.ChunksWritten == 0 {
		t.Fatalf("first build = %+v, want a real rebuild (baseline for the idempotency assertion)", first)
	}

	if code := runIndexCmd(conf, corpus); code != 0 {
		t.Fatalf("runIndexCmd(rerun) = %d, want 0", code)
	}

	second, err := buildIndex(conf, corpus)
	if err != nil {
		t.Fatalf("second buildIndex: %v", err)
	}
	if second.DocsIndexed != 0 {
		t.Errorf("second DocsIndexed = %d, want 0 (same fingerprint must skip, not re-index)", second.DocsIndexed)
	}
	if second.ChunksWritten != 0 {
		t.Errorf("second ChunksWritten = %d, want 0 (same fingerprint must not rewrite chunks)", second.ChunksWritten)
	}
	if second.DocsSkipped != 0 {
		t.Errorf("second DocsSkipped = %d, want 0 (skip-on-fingerprint-hit is not a read failure)", second.DocsSkipped)
	}
	if second.Fingerprint != first.Fingerprint {
		t.Errorf("second Fingerprint = %q, want unchanged %q", second.Fingerprint, first.Fingerprint)
	}
}

// TestRunIndexCmd_WarnsWhenIndexInMemory 修「误导性成功报告」（P5 黄卡 D / CRV Task 8 黄卡 4）：
// db_path 为空时索引落 :memory:，进程退出即蒸发；构建本身成功（退出 0），
// 但必须补一条 WARN，否则运维把 "index built" 当成持久化成功。
// 变异自证：删掉 runIndexCmd 里的 conf.DbPath == "" 告警分支，本用例转红。
func TestRunIndexCmd_WarnsWhenIndexInMemory(t *testing.T) {
	corpus := t.TempDir()
	writeCorpusDoc(t, corpus, "d1.md", "内存索引只在进程存活期内有效。")
	buf := captureSlog(t)

	// 显式零值 Config：DbPath 空 → 索引落 ":memory:"。
	if code := runIndexCmd(config.Config{}, corpus); code != 0 {
		t.Fatalf("runIndexCmd(in-memory) = %d, want 0 (the build itself succeeds)", code)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "in memory only") {
		t.Fatalf("slog output = %q, want a WARN about in-memory-only index", out)
	}
}

// TestEvalCorpusDir_DefaultsToRepoRoot 钉死 -eval 的语料口径（P5 黄卡 E①）：
// 未显式 -corpus 时覆写为仓库根 "."（HITL 口径），显式传入则尊重用户值；
// 覆写开关的唯一来源是 parseFlags 的 corpusExplicit（fs.Visit 结果）。
// 变异自证：把 evalCorpusDir 的默认返回值改回 "docs"，本用例转红。
func TestEvalCorpusDir_DefaultsToRepoRoot(t *testing.T) {
	if got := evalCorpusDir("docs", false); got != "." {
		t.Errorf("evalCorpusDir(docs, not explicit) = %q, want %q (eval corpus is the repo root)", got, ".")
	}
	if got := evalCorpusDir("mydocs", true); got != "mydocs" {
		t.Errorf("evalCorpusDir(mydocs, explicit) = %q, want the user value %q", got, "mydocs")
	}

	f, code := parseFlags([]string{"-eval"})
	if code != 0 {
		t.Fatalf("parseFlags(-eval) = %d, want 0", code)
	}
	if !f.evalRequested {
		t.Error("evalRequested = false, want true for -eval")
	}
	if f.corpusExplicit {
		t.Error("corpusExplicit = true, want false when -corpus is not passed")
	}
	if got := evalCorpusDir(f.corpusDir, f.corpusExplicit); got != "." {
		t.Errorf("wired eval corpus = %q, want %q", got, ".")
	}

	explicit, code := parseFlags([]string{"-eval", "-corpus", "mydocs"})
	if code != 0 {
		t.Fatalf("parseFlags(-eval -corpus mydocs) = %d, want 0", code)
	}
	if !explicit.corpusExplicit {
		t.Error("corpusExplicit = false, want true when -corpus is passed")
	}
	if got := evalCorpusDir(explicit.corpusDir, explicit.corpusExplicit); got != "mydocs" {
		t.Errorf("wired eval corpus = %q, want the explicit %q", got, "mydocs")
	}
}

// TestRunEvalCmd_RejectsMissingOrUnknownGroup 验证 -eval 的 group 校验前置（P5 黄卡 E②）：
// group 缺失/未知都是 harness 故障 → 退出码 1，且校验发生在 eval.Run 最前面
// （先 validate 再 LoadTasks/建 registry/resolveChatter），因此本用例不会发出任何
// LLM 请求，也不需要网络或密钥。
// 变异自证：把 runEvalCmd 的错误分支 return 1 改成 0，本用例转红。
func TestRunEvalCmd_RejectsMissingOrUnknownGroup(t *testing.T) {
	for _, group := range []string{"", "no-such-group"} {
		t.Run("group="+group, func(t *testing.T) {
			code := runEvalCmd(config.Config{}, evalOpts{
				set:      "eval/tasks/retrieval_v1.json",
				group:    group,
				corpus:   ".",
				repeat:   3,
				outDir:   "",
				model:    "deepseek-flash",
				template: "eval-template-v1",
			})
			if code != 1 {
				t.Errorf("runEvalCmd(group=%q) = %d, want 1 (harness failure, no silent 0)", group, code)
			}
		})
	}
}

// withDefaultLogger 兜住 runWithArgs 的全局副作用（YELLOW-1）：它首行就把默认
// logger 换成 JSON→stdout（生产必须），测试里若不还原，后续用例的日志会串到
// stdout，并让 captureSlog 抓不到预期输出。只在测试侧还原，不动生产代码。
func withDefaultLogger(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// captureSlog 把默认 logger 临时重定向到内存 buffer（只收 WARN 及以上），
// 用于断言告警分支确实被走到；测试结束恢复原 handler。
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
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
		{"url+key, model missing", config.Config{EmbedBaseURL: "https://emb.example.com", EmbedAPIKey: "secret"}},
		{"dim missing", config.Config{EmbedBaseURL: "https://emb.example.com", EmbedModel: "m", EmbedDim: 0}},
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

func TestDefaultModel(t *testing.T) {
	if got := defaultModel(""); got != "deepseek-chat" {
		t.Errorf("defaultModel(\"\") = %q, want \"deepseek-chat\"", got)
	}
	if got := defaultModel("custom-model"); got != "custom-model" {
		t.Errorf("defaultModel(\"custom-model\") = %q, want \"custom-model\"", got)
	}
}

func TestNewEmbedder_KeylessAllowed(t *testing.T) {
	emb := newEmbedder(config.Config{
		EmbedBaseURL: "http://localhost:11434",
		EmbedModel:   "nomic-embed-text",
		EmbedDim:     768,
	})
	if emb == nil {
		t.Fatal("newEmbedder with empty EmbedAPIKey = nil, want non-nil Embedder")
	}
}

func TestParseFlags_ModelDefault(t *testing.T) {
	f, code := parseFlags([]string{"-eval"})
	if code != 0 {
		t.Fatalf("parseFlags(-eval) code = %d, want 0", code)
	}
	if f.eval.model != "" {
		t.Errorf("f.eval.model = %q, want empty (so it falls back to defaultModel)", f.eval.model)
	}
	f2, code2 := parseFlags([]string{"-eval", "-model", "custom"})
	if code2 != 0 {
		t.Fatalf("parseFlags(-eval -model custom) code = %d, want 0", code2)
	}
	if f2.eval.model != "custom" {
		t.Errorf("f2.eval.model = %q, want \"custom\"", f2.eval.model)
	}
}
