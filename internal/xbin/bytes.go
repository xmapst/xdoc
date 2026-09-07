// Package xbin 是文件里各种定长二进制形态的编解码：小端整数、96 位十进制数、
// 16 字节标识、100 纳秒计时单位，以及写串之前的合法性检查。
//
// 这些形态是文件格式的一部分，字节布局不能改。
package xbin

import "encoding/binary"

// Uint32LE 按小端读一个 32 位数。
//
// 文件里的多字节整数一律小端，与运行平台无关。
func Uint32LE(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }

// PutUint32LE 按小端写一个 32 位数。
func PutUint32LE(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }

// Uint64LE 按小端读一个 64 位数。
func Uint64LE(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

// PutUint64LE 按小端写一个 64 位数。
func PutUint64LE(b []byte, v uint64) { binary.LittleEndian.PutUint64(b, v) }
