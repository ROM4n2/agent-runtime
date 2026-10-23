package tool

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestEvalArithmetic_Valid(t *testing.T) {
	tests := []struct {
		expr     string
		expected float64
	}{
		{"1 + 2", 3},
		{"10 - 4", 6},
		{"3 * 4", 12},
		{"10 / 2", 5},
		{"7 / 2", 3.5},
		{"1.5 + 2.5", 4.0},
		{"-5 + 3", -2},
		{"2 * -3", -6},
		{"-4 * -2", 8},
		{"2 + 3 * 4", 14},
		{"2 * 3 + 4", 10},
		{"2 * (3 + 4)", 14},
		{"(10 - 2) / (2 + 2)", 2},
		{"((2 + 3) * (4 - 1))", 15},
		{"  12  +   34  ", 46},
		{"100", 100},
		{"-42", -42},
	}

	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			got, err := evalArithmetic(tt.expr)
			if err != nil {
				t.Fatalf("evalArithmetic(%q) unexpected error: %v", tt.expr, err)
			}
			if math.Abs(got-tt.expected) > 1e-9 {
				t.Errorf("evalArithmetic(%q) = %v, want %v", tt.expr, got, tt.expected)
			}
		})
	}
}

func TestEvalArithmetic_Errors(t *testing.T) {
	tests := []struct {
		name        string
		expr        string
		errContains string
	}{
		{"division by zero", "1 / 0", "division by zero"},
		{"division by zero in expr", "10 / (5 - 5)", "division by zero"},
		{"trailing operator", "2 +", "expected number"},
		{"leading operator without operand", "+ 2", "expected number"},
		{"unmatched left paren", "(1 + 2", "missing )"},
		{"unexpected right paren", "1 + )", "expected number"},
		{"double operator", "++", "expected number"},
		{"empty expression", "", "expected number"},
		{"blank spaces", "   ", "expected number"},
		{"invalid character letters", "2 + a", "expected number"},
		{"script injection attempt", "alert(1)", "expected number"},
		{"variable name attempt", "foo * 2", "expected number"},
		{"unsupported symbol", "3 @ 4", "unexpected token"},
		{"trailing numbers", "1 + 2 3", "unexpected token"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := evalArithmetic(tt.expr)
			if err == nil {
				t.Fatalf("evalArithmetic(%q) expected error, got result: %v", tt.expr, got)
			}
			if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("evalArithmetic(%q) error %q does not contain %q", tt.expr, err.Error(), tt.errContains)
			}
		})
	}
}

func TestCalculateTool_Spec(t *testing.T) {
	tool := NewCalculateTool()
	spec := tool.Spec()

	if spec.Type != "function" {
		t.Errorf("spec.Type = %q, want %q", spec.Type, "function")
	}
	if spec.Function.Name != "calculate" {
		t.Errorf("spec.Function.Name = %q, want %q", spec.Function.Name, "calculate")
	}
	if spec.Function.Description == "" {
		t.Error("spec.Function.Description is empty")
	}
	if spec.Function.Parameters == nil {
		t.Error("spec.Function.Parameters is nil")
	}
}

func TestCalculateTool_Execute(t *testing.T) {
	tool := NewCalculateTool()

	// 正常计算
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"expression": "2 * (3 + 4)"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "14" {
		t.Errorf("Execute() = %q, want %q", out, "14")
	}

	// 浮点结果
	out, err = tool.Execute(context.Background(), json.RawMessage(`{"expression": "7 / 2"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "3.5" {
		t.Errorf("Execute() = %q, want %q", out, "3.5")
	}

	// 除零错误
	_, err = tool.Execute(context.Background(), json.RawMessage(`{"expression": "1 / 0"}`))
	if err == nil {
		t.Fatal("expected error for division by zero, got nil")
	}
	if !strings.Contains(err.Error(), "division by zero") {
		t.Errorf("expected error containing 'division by zero', got %v", err)
	}

	// 非法 JSON
	_, err = tool.Execute(context.Background(), json.RawMessage(`{invalid`))
	if err == nil {
		t.Fatal("expected error for invalid json, got nil")
	}

	// 语法错误
	_, err = tool.Execute(context.Background(), json.RawMessage(`{"expression": "2 + "}`))
	if err == nil {
		t.Fatal("expected error for syntax error expression, got nil")
	}
}
