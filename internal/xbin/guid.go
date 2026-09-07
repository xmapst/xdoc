package xbin

import (
	"bytes"
	"encoding/hex"
	"fmt"
)

// Guid 是 16 字节的全局唯一标识，按它的文本形态存放。
//
// 写进文件时前三段要转成小端，见 [Guid.PutBytes]。
type Guid [16]byte

// GuidNil 是全零的 Guid。
var GuidNil Guid

// GuidFromBytes 从 16 字节的存储形态读出一个 Guid。
//
// 前三段（4、2、2 字节）在文件里是小端，这里翻回来；后 8 字节原样。
// 不翻的话，文本形态与别处写出来的对不上。
func GuidFromBytes(b []byte) (Guid, error) {
	if len(b) < 16 {
		return Guid{}, fmt.Errorf("xbin: guid needs 16 bytes, got %d", len(b))
	}
	var g Guid
	g[0], g[1], g[2], g[3] = b[3], b[2], b[1], b[0]
	g[4], g[5] = b[5], b[4]
	g[6], g[7] = b[7], b[6]
	copy(g[8:], b[8:16])
	return g, nil
}

// Bytes 写出 16 字节存储形态。
func (g Guid) Bytes() [16]byte {
	var b [16]byte
	g.PutBytes(b[:])
	return b
}

// PutBytes 把 16 字节存储形态写进 b，前三段转成小端。
func (g Guid) PutBytes(b []byte) {
	_ = b[15]
	b[0], b[1], b[2], b[3] = g[3], g[2], g[1], g[0]
	b[4], b[5] = g[5], g[4]
	b[6], b[7] = g[7], g[6]
	copy(b[8:16], g[8:])
}

// String 写成 8-4-4-4-12 的小写十六进制。
func (g Guid) String() string {
	var buf [36]byte
	hex.Encode(buf[0:8], g[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], g[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], g[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], g[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], g[10:16])
	return string(buf[:])
}

// ParseGuid 解析 8-4-4-4-12 形式的文本，长度与分隔符都要对。
func ParseGuid(s string) (Guid, error) {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return Guid{}, fmt.Errorf("xbin: invalid guid %q: want 8-4-4-4-12 hex", s)
	}
	var g Guid
	for _, seg := range [...]struct {
		src, dst int
		n        int
	}{
		{0, 0, 4}, {9, 4, 2}, {14, 6, 2}, {19, 8, 2}, {24, 10, 6},
	} {
		if _, err := hex.Decode(g[seg.dst:seg.dst+seg.n], []byte(s[seg.src:seg.src+seg.n*2])); err != nil {
			return Guid{}, fmt.Errorf("xbin: invalid guid %q: %w", s, err)
		}
	}
	return g, nil
}

// Compare 按 16 字节逐字节比较。
//
// 比的是文本形态的字节序，不是存储形态——排序结果因此与文本序一致。
func (g Guid) Compare(o Guid) int {
	return bytes.Compare(g[:], o[:])
}
