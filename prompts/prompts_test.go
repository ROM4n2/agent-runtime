package prompts_test

import (
	"agent-runtime/prompts"
	"strings"
	"testing"
)

func TestGet_EvalV1(t *testing.T) {
	content, err := prompts.Get(prompts.EvalV1)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if content != "" {
		t.Errorf("expected empty content for %s, got: %q", prompts.EvalV1, content)
	}
}

func TestGet_EvalV2(t *testing.T) {
	content, err := prompts.Get(prompts.EvalV2)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if content == "" {
		t.Fatalf("expected non-empty content for %s, got empty", prompts.EvalV2)
	}
	if !strings.Contains(content, "doc_id") {
		t.Errorf("expected %s to contain %q, but did not. Content: %s", prompts.EvalV2, "doc_id", content)
	}
	if !strings.Contains(content, "search_docs") {
		t.Errorf("expected %s to contain %q, but did not. Content: %s", prompts.EvalV2, "search_docs", content)
	}
}

func TestGet_DefaultV1(t *testing.T) {
	content, err := prompts.Get(prompts.DefaultVersion)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if content == "" {
		t.Errorf("expected non-empty content for %s, got empty", prompts.DefaultVersion)
	}
}

func TestGet_NonExistentVersion(t *testing.T) {
	_, err := prompts.Get("non-existent-version")
	if err == nil {
		t.Fatalf("expected error for non-existent-version, got nil")
	}
	expectedMsg := `prompts: unknown template version "non-existent-version"`
	if err.Error() != expectedMsg {
		t.Errorf("expected error message %q, got %q", expectedMsg, err.Error())
	}
}

func TestGet_EmptyVersion(t *testing.T) {
	_, err := prompts.Get("")
	if err == nil {
		t.Fatalf("expected error for empty version, got nil")
	}
	expectedMsg := "prompts: empty template version"
	if err.Error() != expectedMsg {
		t.Errorf("expected error message %q, got %q", expectedMsg, err.Error())
	}
}
