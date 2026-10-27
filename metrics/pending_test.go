package metrics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPendingSourceExposedAsGauge 锁死 agent_tasks_pending 的存在与取值：
// 它是 /metrics 上唯一能反映「队列积压」的指标，缺它时排障只能看
// agent_tasks_running（在飞数），而积压恰恰是 running 之外的排队任务。
func TestPendingSourceExposedAsGauge(t *testing.T) {
	SetPendingSource(func(context.Context) (int64, error) { return 7, nil })
	t.Cleanup(func() { SetPendingSource(nil) })

	body := scrape(t)
	if !strings.Contains(body, "# TYPE agent_tasks_pending gauge") {
		t.Fatalf("missing pending gauge declaration; got:\n%s", body)
	}
	if !strings.Contains(body, "agent_tasks_pending 7") {
		t.Fatalf("pending gauge value wrong; got:\n%s", body)
	}
}

// TestPendingSourceErrorDoesNotBreakScrape 验证「抓取失败不能拖垮整个端点」：
// 存储故障时 /metrics 仍必须返回 200 与其余指标，积压值退化为 -1（未知），
// 而不是让监控端整页 500 —— 那会让排障者失去所有其他信号。
func TestPendingSourceErrorDoesNotBreakScrape(t *testing.T) {
	SetPendingSource(func(context.Context) (int64, error) {
		return 0, errors.New("store unavailable")
	})
	t.Cleanup(func() { SetPendingSource(nil) })

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when pending source fails", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "agent_tasks_pending -1") {
		t.Fatalf("failed source must degrade to -1 (unknown); got:\n%s", body)
	}
	// 其余指标必须照常输出，证明故障被局部隔离。
	if !strings.Contains(body, "agent_tasks_submitted") {
		t.Fatalf("other metrics lost when pending source failed; got:\n%s", body)
	}
}

// TestNoPendingSourceOmitsGauge 验证未装配 source（如 -index / -eval 子命令
// 这类不跑 worker 的进程）时不输出该指标，避免暴露一个恒为 0 的假积压。
func TestNoPendingSourceOmitsGauge(t *testing.T) {
	SetPendingSource(nil)
	body := scrape(t)
	if strings.Contains(body, "agent_tasks_pending") {
		t.Fatalf("pending gauge must be omitted when no source is wired; got:\n%s", body)
	}
}

func scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}
