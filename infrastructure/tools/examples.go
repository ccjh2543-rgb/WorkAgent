package tools

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// CalculatorTool 示例工具：数学计算器。
// 输入两个数字用逗号分隔，返回相加结果。只读无副作用，注册时自动放行。
type CalculatorTool struct{}

// Name 返回工具名
func (c *CalculatorTool) Name() string { return "calculator" }

// Description 返回工具说明
func (c *CalculatorTool) Description() string {
	return "数学计算器，输入两个数字用逗号分隔，例如：10,20，返回相加结果"
}

// Run 执行计算，参数为两个逗号分隔的数字
func (c *CalculatorTool) Run(args string) (string, error) {
	parts := strings.Split(args, ",")
	if len(parts) != 2 {
		return "", errors.New("参数错误，需要两个数字")
	}
	a, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return "", err
	}
	b, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return "", err
	}
	res := a + b
	return fmt.Sprintf("%d + %d = %d", a, b, res), nil
}

// SearchMockTool 示例工具：模拟联网搜索。只读无副作用，注册时自动放行。
type SearchMockTool struct{}

// Name 返回工具名
func (s *SearchMockTool) Name() string { return "search" }

// Description 返回工具说明
func (s *SearchMockTool) Description() string {
	return "模拟联网搜索工具，传入搜索关键词"
}

// Run 执行模拟搜索
func (s *SearchMockTool) Run(args string) (string, error) {
	return fmt.Sprintf("【模拟搜索结果】关于「%s」的资料：xxxx信息", args), nil
}
