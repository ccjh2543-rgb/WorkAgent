package bench

import (
	"encoding/xml"
	"fmt"
	"os"
	"strings"

	domaineval "simpleAgent/domain/eval"
)

// junitRoot 同时兼容 <testsuites> 与 <testsuite> 两种根节点。
type junitRoot struct {
	XMLName xml.Name
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Errors   int          `xml:"errors,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Cases    []junitCase  `xml:"testcase"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitCase struct {
	Name      string    `xml:"name,attr"`
	ClassName string    `xml:"classname,attr"`
	Failure   *junitMsg `xml:"failure"`
	Error     *junitMsg `xml:"error"`
	Skipped   *junitMsg `xml:"skipped"`
}

type junitMsg struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

// ParseJUnit 解析 pytest 生成的 JUnit XML，返回用例计数与失败详情。
//
// passed = tests - failures - errors - skipped；计数以 XML 为准，
// 不依赖 pytest 退出码。
func ParseJUnit(path string) (domaineval.VerifierResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return domaineval.VerifierResult{}, fmt.Errorf("读取 JUnit XML 失败: %w", err)
	}
	var root junitRoot
	if err := xml.Unmarshal(data, &root); err != nil {
		return domaineval.VerifierResult{}, fmt.Errorf("解析 JUnit XML 失败: %w", err)
	}
	var suites []junitSuite
	switch root.XMLName.Local {
	case "testsuite":
		var single junitSuite
		if err := xml.Unmarshal(data, &single); err != nil {
			return domaineval.VerifierResult{}, fmt.Errorf("解析 JUnit testsuite 失败: %w", err)
		}
		suites = []junitSuite{single}
	case "testsuites":
		suites = root.Suites
	default:
		return domaineval.VerifierResult{}, fmt.Errorf("未知 JUnit 根节点: %s", root.XMLName.Local)
	}

	res := domaineval.VerifierResult{ResultXML: path}
	for _, s := range suites {
		aggregateSuite(s, &res)
	}
	if res.Tests == 0 {
		res.Tests = len(res.Cases)
	}
	return res, nil
}

// aggregateSuite 递归累加测试套件，并把失败用例转成可读记录。
func aggregateSuite(s junitSuite, res *domaineval.VerifierResult) {
	res.Tests += s.Tests
	res.Failures += s.Failures
	res.Errors += s.Errors
	res.Skipped += s.Skipped
	if s.Tests == 0 {
		res.Tests += len(s.Cases)
	}
	for _, c := range s.Cases {
		name := c.ClassName
		if name != "" {
			name += "."
		}
		name += c.Name
		status := "passed"
		detail := ""
		switch {
		case c.Error != nil:
			status = "error"
			detail = msgDetail(c.Error)
		case c.Failure != nil:
			status = "failure"
			detail = msgDetail(c.Failure)
		case c.Skipped != nil:
			status = "skipped"
			detail = msgDetail(c.Skipped)
		}
		if status != "passed" {
			res.Cases = append(res.Cases, domaineval.CaseResult{Name: name, Status: status, Detail: detail})
		}
	}
	// 嵌套 suite 的计数通常已包含在父级 tests 中，只在父级无显式计数时累加。
	if s.Tests == 0 && s.Failures == 0 && s.Errors == 0 {
		for _, child := range s.Suites {
			aggregateSuite(child, res)
		}
	}
}

// msgDetail 提取失败信息并截断，避免报告膨胀。
func msgDetail(m *junitMsg) string {
	text := strings.TrimSpace(m.Text)
	if text == "" {
		text = strings.TrimSpace(m.Message)
	}
	if text == "" {
		return ""
	}
	r := []rune(text)
	if len(r) > 500 {
		return string(r[:500]) + "..."
	}
	return text
}
