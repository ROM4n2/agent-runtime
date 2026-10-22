package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerExposesCounters(t *testing.T) {
	// 干净环境：只计一次提交与一次完成
	IncSubmitted()
	IncDone()

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"# TYPE agent_tasks_submitted counter",
		"agent_tasks_submitted 1",
		"agent_tasks_running 0",
		"agent_tasks_done 1",
		"agent_tasks_failed 0",
		"agent_tasks_recovered 0",
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

func TestIncRecoveredRestoresRunningGauge(t *testing.T) {
	beforeRunning := running.Load()
	beforeRecovered := recovered.Load()

	IncRecovered(2)

	if got := running.Load(); got != beforeRunning+2 {
		t.Errorf("running = %d, want %d", got, beforeRunning+2)
	}
	if got := recovered.Load(); got != beforeRecovered+2 {
		t.Errorf("recovered = %d, want %d", got, beforeRecovered+2)
	}

	IncRecovered(0) // no-op，不得改变 gauge
	if got := running.Load(); got != beforeRunning+2 {
		t.Errorf("IncRecovered(0) changed running to %d", got)
	}
}
