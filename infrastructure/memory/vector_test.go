package memory

import (
	"math"
	"testing"
)

// TestCosineSimilarity 覆盖余弦相似度的归一化与边界情况
func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float32
	}{
		{"同向向量相似度为 1", []float32{1, 2, 3}, []float32{4, 5, 6}, 0.9746318},
		{"相同向量相似度为 1", []float32{1, 2, 3}, []float32{1, 2, 3}, 1},
		{"同向不同模长相似度仍为 1", []float32{1, 2}, []float32{2, 4}, 1},
		{"反向向量相似度为 -1", []float32{1, 0}, []float32{-1, 0}, -1},
		{"正交向量相似度为 0", []float32{1, 0}, []float32{0, 1}, 0},
		{"零向量返回 0", []float32{0, 0}, []float32{1, 1}, 0},
		{"长度不一致返回 0", []float32{1, 2}, []float32{1}, 0},
		{"空向量返回 0", nil, nil, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CosineSimilarity(tt.a, tt.b)
			if math.Abs(float64(got-tt.want)) > 1e-6 {
				t.Fatalf("相似度不匹配: got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestVectorEncodeDecode 验证向量字节编码的往返一致性
func TestVectorEncodeDecode(t *testing.T) {
	original := []float32{0, 1, -1, 3.14159, 1e10, -1e-10}

	encoded := EncodeVector(original)
	if len(encoded) != len(original)*4 {
		t.Fatalf("编码长度不匹配: got %d, want %d", len(encoded), len(original)*4)
	}

	decoded, err := DecodeVector(encoded)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if len(decoded) != len(original) {
		t.Fatalf("解码长度不匹配: got %d, want %d", len(decoded), len(original))
	}
	for i := range original {
		if decoded[i] != original[i] {
			t.Fatalf("第 %d 个分量不匹配: got %v, want %v", i, decoded[i], original[i])
		}
	}
}

// TestDecodeVectorRejectsInvalidLength 非法字节长度必须报错而不是静默截断
func TestDecodeVectorRejectsInvalidLength(t *testing.T) {
	if _, err := DecodeVector([]byte{1, 2, 3}); err == nil {
		t.Fatal("字节长度不是 4 的整数倍时应返回错误")
	}
}

// TestTextHash 内容或章节任一变化都必须产生新哈希，否则向量会被错误复用
func TestTextHash(t *testing.T) {
	base := TextHash("架构约定", "严格分层")

	if base != TextHash("架构约定", "严格分层") {
		t.Fatal("相同内容的哈希应当一致")
	}
	if base == TextHash("架构约定", "严格分层2") {
		t.Fatal("正文变化后哈希应当不同")
	}
	if base == TextHash("沟通偏好", "严格分层") {
		t.Fatal("章节变化后哈希应当不同")
	}
}

// TestEmbedText 向量化文本应包含章节，以提升主题相关性
func TestEmbedText(t *testing.T) {
	if got := EmbedText("架构约定", "严格分层"); got != "架构约定 严格分层" {
		t.Fatalf("向量化文本不匹配: %q", got)
	}
	if got := EmbedText("", "严格分层"); got != "严格分层" {
		t.Fatalf("空章节时不应拼接前缀: %q", got)
	}
}

// TestTokenText 索引词元串不应混入 OR 等查询语法词
func TestTokenText(t *testing.T) {
	got := TokenText("架构约定")
	if got != "架构 构约 约定" {
		t.Fatalf("词元串不匹配: %q", got)
	}
	if got := TokenText("a b"); got != "" {
		t.Fatalf("无可用词元时应返回空串: %q", got)
	}
}
