package tool

import (
	"agent-runtime/llm"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type calculateTool struct{}

// NewCalculateTool 返回四则运算工具实例。
func NewCalculateTool() Tool {
	return &calculateTool{}
}

func (c *calculateTool) Spec() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.FunctionSpec{
			Name:        "calculate",
			Description: "Evaluate a basic arithmetic expression. Supports + - * / and parentheses, e.g. \"2*(3+4)\".",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"expression": map[string]any{"type": "string", "description": "The arithmetic expression to evaluate."},
				},
				"required": []string{"expression"},
			},
		},
	}
}

// Execute 实现 Tool 接口。ctx 当前未被使用（纯 CPU 求值无中断点），
// 保留参数是为了让取消语义可统一穿透到所有工具（接口只加不改）。
func (c *calculateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Expression string `json:"expression"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	v, err := evalArithmetic(p.Expression)
	if err != nil {
		return "", err
	}
	return strconv.FormatFloat(v, 'f', -1, 64), nil
}

// evalArithmetic 安全地求值四则运算表达式，只允许数字、运算符与括号，
// 不使用 eval/反射，从根本上杜绝代码注入。
func evalArithmetic(s string) (float64, error) {
	p := &parser{src: []rune(strings.TrimSpace(s))}
	v, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	if p.pos != len(p.src) {
		return 0, fmt.Errorf("unexpected token at %d", p.pos)
	}
	return v, nil
}

type parser struct {
	src []rune
	pos int
}

func (p *parser) peek() rune {
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *parser) skipSpace() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) parseExpr() (float64, error) {
	p.skipSpace()
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpace()
		op := p.peek()
		if op == '+' || op == '-' {
			p.pos++
			right, err := p.parseTerm()
			if err != nil {
				return 0, err
			}
			if op == '+' {
				left += right
			} else {
				left -= right
			}
			continue
		}
		return left, nil
	}
}

func (p *parser) parseTerm() (float64, error) {
	p.skipSpace()
	left, err := p.parseFactor()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpace()
		op := p.peek()
		if op == '*' || op == '/' {
			p.pos++
			right, err := p.parseFactor()
			if err != nil {
				return 0, err
			}
			if op == '*' {
				left *= right
			} else {
				if right == 0 {
					return 0, errors.New("division by zero")
				}
				left /= right
			}
			continue
		}
		return left, nil
	}
}

func (p *parser) parseFactor() (float64, error) {
	p.skipSpace()
	if p.peek() == '-' {
		p.pos++
		v, err := p.parseFactor()
		if err != nil {
			return 0, err
		}
		return -v, nil
	}
	if p.peek() == '(' {
		p.pos++
		v, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		p.skipSpace()
		if p.peek() != ')' {
			return 0, errors.New("missing )")
		}
		p.pos++
		return v, nil
	}
	start := p.pos
	for p.pos < len(p.src) && ((p.src[p.pos] >= '0' && p.src[p.pos] <= '9') || p.src[p.pos] == '.') {
		p.pos++
	}
	if p.pos == start {
		return 0, fmt.Errorf("expected number at %d", p.pos)
	}
	return strconv.ParseFloat(string(p.src[start:p.pos]), 64)
}
