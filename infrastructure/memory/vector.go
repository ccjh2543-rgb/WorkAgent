package memory

import (
	"encoding/binary"
	"fmt"
	"math"
)

// CosineSimilarity 计算两个等长向量的余弦相似度，取值范围 [-1, 1]。
// 长度不一致或存在零向量（模长为 0）时返回 0，避免除零与越界。
func CosineSimilarity(a, b []float32) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		normA += x * x
		normB += y * y
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}

// EncodeVector 把 float32 向量编码为小端序字节串，用于存入 SQLite BLOB 字段
func EncodeVector(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

// DecodeVector 把 EncodeVector 产生的字节串还原为 float32 向量
func DecodeVector(buf []byte) ([]float32, error) {
	if len(buf)%4 != 0 {
		return nil, fmt.Errorf("向量字节长度 %d 不是 4 的整数倍", len(buf))
	}
	vec := make([]float32, len(buf)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return vec, nil
}
