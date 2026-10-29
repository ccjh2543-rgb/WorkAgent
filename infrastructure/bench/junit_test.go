package bench

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseJUnit 覆盖 testsuites/testsuite 两种根节点与失败用例提取。
func TestParseJUnit(t *testing.T) {
	cases := []struct {
		name         string
		content      string
		wantTests    int
		wantFailures int
		wantCases    int
		wantErr      bool
	}{
		{
			name: "多个 testsuite",
			content: `<?xml version="1.0" encoding="utf-8"?>
<testsuites>
  <testsuite name="a" tests="2" failures="1" errors="0" skipped="0">
    <testcase classname="t" name="ok"/>
    <testcase classname="t" name="bad"><failure message="断言失败">detail</failure></testcase>
  </testsuite>
  <testsuite name="b" tests="1" failures="0" errors="0" skipped="1">
    <testcase classname="t" name="skip"><skipped message="跳过"/></testcase>
  </testsuite>
</testsuites>`,
			wantTests:    3,
			wantFailures: 1,
			wantCases:    2,
		},
		{
			name: "单个 testsuite",
			content: `<testsuite name="a" tests="1" failures="0" errors="0" skipped="0">
  <testcase classname="t" name="ok"/>
</testsuite>`,
			wantTests:    1,
			wantFailures: 0,
			wantCases:    0,
		},
		{
			name:    "非法 XML",
			content: `<testsuite`,
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "results.xml")
			if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
				t.Fatalf("写入失败: %v", err)
			}
			res, err := ParseJUnit(path)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if res.Tests != c.wantTests || res.Failures != c.wantFailures || len(res.Cases) != c.wantCases {
				t.Fatalf("tests=%d failures=%d cases=%d, want %d/%d/%d",
					res.Tests, res.Failures, len(res.Cases), c.wantTests, c.wantFailures, c.wantCases)
			}
		})
	}
}
