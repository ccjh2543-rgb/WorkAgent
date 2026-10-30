package memory

import (
	"strings"
	"testing"
)

// TestValidateCandidate 表驱动覆盖长度、控制字符与凭证三类拒绝原因。
func TestValidateCandidate(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr bool
	}{
		{name: "正常约定", content: "domain 层不依赖任何外部包"},
		{name: "空内容", content: "   ", wantErr: true},
		{name: "超长内容", content: strings.Repeat("长", MaxCandidateRunes+1), wantErr: true},
		{name: "控制字符", content: "正常内容\x00尾", wantErr: true},
		{name: "私钥块", content: "-----BEGIN RSA PRIVATE KEY-----\nabc", wantErr: true},
		{name: "OpenAI 密钥", content: "配置 sk-abcdefghijklmnopqrstuvwxyz 即可", wantErr: true},
		{name: "AWS 密钥", content: "AKIAIOSFODNN7EXAMPLE", wantErr: true},
		{name: "密码赋值", content: "password = hunter2", wantErr: true},
		{name: "令牌赋值", content: "api_key=abcdefgh12345678", wantErr: true},
		{name: "描述性提到密码", content: "配置文件里的密码字段必须走环境变量注入"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCandidate(tc.content)
			if tc.wantErr && err == nil {
				t.Fatalf("期望拒绝，实际通过")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望通过，实际拒绝: %v", err)
			}
		})
	}
}

// TestValidateSection 验证章节名的长度与换行约束。
func TestValidateSection(t *testing.T) {
	if err := ValidateSection(""); err != nil {
		t.Fatalf("空章节应交给调用方归入默认值: %v", err)
	}
	if err := ValidateSection("架构约定"); err != nil {
		t.Fatalf("正常章节应通过: %v", err)
	}
	if err := ValidateSection("换\n行"); err == nil {
		t.Fatal("包含换行的章节应被拒绝")
	}
	if err := ValidateSection(strings.Repeat("长", MaxSectionRunes+1)); err == nil {
		t.Fatal("超长章节应被拒绝")
	}
}
