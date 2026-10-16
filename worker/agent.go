package worker

import (
	"agent-api/llm"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxAgentSteps 限制 agent 单轮任务的工具调用轮数，防止模型陷入无限工具循环。
const maxAgentSteps = 5

// toolHandler 执行一个工具，args 是模型传来的 JSON 参数串。
type toolHandler func(args json.RawMessage) (string, error)

// defaultTools 返回注册给模型的工具描述与对应的本地执行器。
// 两者通过函数名关联：模型返回 tool_call.function.name，worker 据此查 handlers。
func defaultTools() (specs []llm.Tool, handlers map[string]toolHandler) {
	handlers = map[string]toolHandler{
		"calculate":    calcTool,
		"current_time": timeTool,
	}
	specs = []llm.Tool{
		{
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
		},
		{
			Type: "function",
			Function: llm.FunctionSpec{
				Name:        "current_time",
				Description: "Return the current date and time. Optionally pass an IANA timezone name, e.g. \"Asia/Shanghai\".",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"timezone": map[string]any{"type": "string", "description": "IANA timezone name. Defaults to UTC."},
					},
				},
			},
		},
	}
	return specs, handlers
}

// runAgent 执行 think→call tool→observe 的多步循环，直到模型给出终态文本或步数耗尽。
// exec 根据工具名执行本地实现，返回结果字符串（出错也返回字符串，让模型自行纠正）。
func runAgent(ctx context.Context, c chatter, prompt string, tools []llm.Tool, exec func(llm.ToolCall) string, onStep func(TraceStep)) (string, error) {
	turns := []llm.Message{{Role: "user", Content: prompt}}

	step := 0
	emit := func(kind, tool, detail, callID string) {
		if onStep != nil {
			onStep(TraceStep{Step: step, Kind: kind, Tool: tool, Detail: detail, CallID: callID})
		}
	}

	for ; step < maxAgentSteps; step++ {
		// 上下文取消（停机/超时）会透传到每次 LLM 调用
		resp, err := c.ChatWithTools(ctx, turns, tools)
		if err != nil {
			emit(KindError, "", "upstream error", "")
			return "", err
		}

		// 每轮 LLM 返回后，若 resp.Content 不为空，emit think
		if resp.Content != "" {
			emit(KindThink, "", resp.Content, "")
		}

		// 没有工具调用 = 模型认为可以收尾，返回最终文本
		if len(resp.ToolCalls) == 0 {
			emit(KindFinal, "", resp.Content, "")
			return resp.Content, nil
		}

		// 把模型的"意图"原样记回对话，工具结果随后以 role=tool 追加
		turns = append(turns, llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})
		for _, tc := range resp.ToolCalls {
			emit(KindToolCall, tc.Function.Name, tc.Function.Arguments, tc.ID)
			res := exec(tc)
			emit(KindToolResult, tc.Function.Name, res, tc.ID)
			turns = append(turns, llm.Message{
				Role:       "tool",
				Content:    res,
				ToolCallID: tc.ID,
			})
		}
	}

	emit(KindError, "", "max steps exceeded", "")
	return "", errors.New("agent exceeded max steps")
}

// ---- 工具实现 ----

func calcTool(args json.RawMessage) (string, error) {
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

func timeTool(args json.RawMessage) (string, error) {
	var p struct {
		Timezone string `json:"timezone"`
	}
	_ = json.Unmarshal(args, &p) // timezone 可选，忽略解析错误
	loc := time.UTC
	if p.Timezone != "" {
		if l, err := time.LoadLocation(p.Timezone); err == nil {
			loc = l
		}
	}
	return time.Now().In(loc).Format(time.RFC3339), nil
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
