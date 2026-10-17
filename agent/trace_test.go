package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestTraceStep_JSONContract(t *testing.T) {
	// 1. Verify constants
	if KindThink != "think" {
		t.Errorf("expected KindThink to be 'think', got %q", KindThink)
	}
	if KindToolCall != "tool_call" {
		t.Errorf("expected KindToolCall to be 'tool_call', got %q", KindToolCall)
	}
	if KindToolResult != "tool_result" {
		t.Errorf("expected KindToolResult to be 'tool_result', got %q", KindToolResult)
	}
	if KindFinal != "final" {
		t.Errorf("expected KindFinal to be 'final', got %q", KindFinal)
	}
	if KindError != "error" {
		t.Errorf("expected KindError to be 'error', got %q", KindError)
	}

	// 2. Full fields serialization
	full := TraceStep{
		Step:   1,
		Kind:   KindToolCall,
		Tool:   "calculator",
		Detail: "add(2, 3)",
		CallID: "call_abc123",
	}
	data, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("failed to marshal full TraceStep: %v", err)
	}
	jsonStr := string(data)
	for _, expectedKey := range []string{`"step"`, `"kind"`, `"tool"`, `"detail"`, `"call_id"`} {
		if !strings.Contains(jsonStr, expectedKey) {
			t.Errorf("expected json to contain %s, got: %s", expectedKey, jsonStr)
		}
	}

	// 3. Omitempty fields when empty
	emptyFields := TraceStep{
		Step: 2,
		Kind: KindThink,
	}
	emptyData, err := json.Marshal(emptyFields)
	if err != nil {
		t.Fatalf("failed to marshal empty TraceStep: %v", err)
	}
	emptyJSONStr := string(emptyData)
	if !strings.Contains(emptyJSONStr, `"step"`) || !strings.Contains(emptyJSONStr, `"kind"`) {
		t.Errorf("expected json to contain step and kind, got: %s", emptyJSONStr)
	}
	for _, omittedKey := range []string{`"tool"`, `"detail"`, `"call_id"`} {
		if strings.Contains(emptyJSONStr, omittedKey) {
			t.Errorf("expected json NOT to contain %s when empty, got: %s", omittedKey, emptyJSONStr)
		}
	}
}

func TestTraceBuffer_OverflowDropsOldest(t *testing.T) {
	buf := NewTraceBuffer(32)
	for i := 0; i < 40; i++ {
		buf.Append(TraceStep{
			Step:   i,
			Kind:   KindThink,
			Detail: fmt.Sprintf("step %d", i),
		})
	}

	snap := buf.Snapshot()
	if len(snap) != 32 {
		t.Fatalf("expected snapshot len 32, got %d", len(snap))
	}

	if snap[0].Step != 8 {
		t.Errorf("expected first step in snapshot to be 8, got %d", snap[0].Step)
	}
	if snap[len(snap)-1].Step != 39 {
		t.Errorf("expected last step in snapshot to be 39, got %d", snap[len(snap)-1].Step)
	}
}

func TestTraceBuffer_SnapshotIsCopy(t *testing.T) {
	buf := NewTraceBuffer(32)
	buf.Append(TraceStep{
		Step:   1,
		Kind:   KindThink,
		Detail: "original",
	})

	snap1 := buf.Snapshot()
	if len(snap1) != 1 {
		t.Fatalf("expected 1 element, got %d", len(snap1))
	}
	snap1[0].Detail = "mutated"

	snap2 := buf.Snapshot()
	if len(snap2) != 1 {
		t.Fatalf("expected 1 element, got %d", len(snap2))
	}
	if snap2[0].Detail != "original" {
		t.Errorf("expected snap2[0].Detail to remain 'original', got %q", snap2[0].Detail)
	}
}

func TestTraceBuffer_TruncatesDetail(t *testing.T) {
	buf := NewTraceBuffer(32)
	longDetail := strings.Repeat("x", 600)
	buf.Append(TraceStep{
		Step:   1,
		Kind:   KindToolResult,
		Detail: longDetail,
	})

	snap := buf.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 element, got %d", len(snap))
	}
	if len(snap[0].Detail) != MaxTraceDetail {
		t.Errorf("expected Detail length %d, got %d", MaxTraceDetail, len(snap[0].Detail))
	}
	if snap[0].Detail != strings.Repeat("x", MaxTraceDetail) {
		t.Errorf("Detail content does not match truncated prefix")
	}
}

func TestTraceBuffer_ConcurrentAppend(t *testing.T) {
	buf := NewTraceBuffer(32)
	const goroutines = 10
	const perGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				buf.Append(TraceStep{
					Step:   gid*1000 + i,
					Kind:   KindThink,
					Detail: fmt.Sprintf("worker %d item %d", gid, i),
				})
				_ = buf.Snapshot()
			}
		}(g)
	}
	wg.Wait()

	snap := buf.Snapshot()
	if len(snap) != 32 {
		t.Errorf("expected final snapshot length 32, got %d", len(snap))
	}
}
