package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerExposesCounters(t *testing.T) {
	// 干净环境：计一次提交、一次抢占、一次完成。
	// running 的入口是 IncClaimed（抢占），不是 IncSubmitted（提交入队）。
	IncSubmitted()
	IncClaimed()
	IncDone()

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"# TYPE agent_tasks_submitted counter",
		"agent_tasks_running 0",
		"# TYPE agent_tasks_done counter",
		"# TYPE agent_tasks_failed counter",
		"# TYPE agent_tasks_recovered counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q; got:\n%s", want, body)
		}
	}
}

func TestCounterMonotonic(t *testing.T) {
	before := done.Load()
	IncDone()
	if done.Load() != before+1 {
		t.Fatalf("done counter not incremented")
	}
}

// TestIncRecoveredCountsRecoveriesOnly 验证崩溃恢复只增加 recovered 计数：
// 恢复的任务回到 pending 排队，其 running 由抢占时的 IncClaimed 计入。
// （旧语义曾在此直接 +running，导致重启后 running 虚高。）
func TestIncRecoveredCountsRecoveriesOnly(t *testing.T) {
	beforeRunning := running.Load()
	beforeRecovered := recovered.Load()

	IncRecovered(2)

	if got := recovered.Load(); got != beforeRecovered+2 {
		t.Errorf("recovered = %d, want %d", got, beforeRecovered+2)
	}
	if got := running.Load(); got != beforeRunning {
		t.Errorf("IncRecovered 不得改变 running：got %d, want %d", got, beforeRunning)
	}

	IncRecovered(0) // no-op，不得改变任何计数
	if got := recovered.Load(); got != beforeRecovered+2 {
		t.Errorf("IncRecovered(0) changed recovered to %d", got)
	}
}
