package session

import "testing"

// TestRedact 覆盖各类敏感信息的脱敏。落盘的内容一旦泄露就是永久的，
// 因此这里对边界情况要求比较严。
func TestRedact(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"空串原样返回", "", ""},
		{"普通文本不变", "这段文本没有任何敏感信息", "这段文本没有任何敏感信息"},
		{"短横线密钥串", "key 是 sk-test-placeholder-0000000000", "key 是 [已脱敏:密钥]"},
		{
			"带前缀的赋值形式保留完整键名",
			"MEMORY_EMBEDDING_API_KEY=sk-test-embedding-0000000000",
			"MEMORY_EMBEDDING_API_KEY=[已脱敏:密钥]",
		},
		{"冒号形式", "api_key: abcdef12345678", "api_key=[已脱敏:密钥]"},
		{"password 字段", "password=SuperSecret123", "password=[已脱敏:密钥]"},
		{"值带引号", `token="abcdef12345678"`, "token=[已脱敏:密钥]"},
		{"阿里云 AccessKeyId", "LTAItestplaceholder000", "[已脱敏:AccessKey]"},
		{"Bearer 令牌", "Authorization: Bearer abcdefghijklmnop123", "Authorization: [已脱敏:令牌]"},
		{"手机号", "联系电话 13812345678", "联系电话 [已脱敏:手机号]"},
		{"过短的值不算密钥", "sk-abc", "sk-abc"},
		{"键名太长不算密钥", "monkey=abcdef12345678", "monkey=abcdef12345678"},
		{"普通等号赋值不受影响", "count=123456789", "count=123456789"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Redact(tt.input); got != tt.want {
				t.Fatalf("脱敏结果不匹配:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

// TestRedactMultipleSecrets 一条消息里出现多个敏感信息时都要被掩掉
func TestRedactMultipleSecrets(t *testing.T) {
	input := "api_key=abcdef12345678 与 sk-test-placeholder-0000000000 都要处理"
	want := "api_key=[已脱敏:密钥] 与 [已脱敏:密钥] 都要处理"
	if got := Redact(input); got != want {
		t.Fatalf("多敏感信息脱敏结果不匹配:\n got %q\nwant %q", got, want)
	}
}

// TestRedactIsIdempotent 已脱敏的文本再次脱敏不应被继续破坏
func TestRedactIsIdempotent(t *testing.T) {
	once := Redact("api_key=abcdef12345678")
	if twice := Redact(once); twice != once {
		t.Fatalf("重复脱敏不应改变结果: %q -> %q", once, twice)
	}
}
