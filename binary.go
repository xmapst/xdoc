package xdoc

import (
	"time"

	"github.com/xmapst/xdoc/internal/xbin"
)

// Decimal 是十进制定点数：96 位尾数配一个 0..28 的小数位数，
// 取值 (-1)^符号 × 尾数 / 10^小数位数。
//
// 小数位数是值的一部分而不是显示选项：1 与 1.00 相等，但落盘是不同的 16 个字节，
// 打印出来也不同。用只存"值"的类型做中转会把这一位丢掉。
type Decimal = xbin.Decimal

// DecimalFromParts 按 96 位尾数的三个 32 位字、小数位数与符号构造。
//
// 只校验小数位数不超过 28；尾数与符号不联动，尾数全零配 neg 为真得到负零——
// 它打印成 "0"、比较时等于正零，只有落盘的 16 字节里差一位。
func DecimalFromParts(lo, mid, hi uint32, scale uint8, neg bool) (Decimal, error) {
	return xbin.DecimalFromParts(lo, mid, hi, scale, neg)
}

// DecimalFromBytes 从 16 字节的落盘形态还原。
//
// 布局是四个小端 int32：lo、mid、hi、flags；flags 里只有 bit16..23（小数位数）
// 与 bit31（符号）有效。保留位不为 0 一律报错，不解出一个看似正常的数继续往下算。
func DecimalFromBytes(b []byte) (Decimal, error) { return xbin.DecimalFromBytes(b) }

// Guid 是 16 字节的标识。内存里按大端存，落盘时前三段（4+2+2 字节）翻成小端，
// 后 8 字节原样——翻转只发生在进出文件的关口上。
type Guid = xbin.Guid

// GuidNil 是全零的 Guid，可以拿来判一个值有没有被赋过。
var GuidNil Guid = xbin.GuidNil

// ParseGuid 解析 8-4-4-4-12 的十六进制写法。
func ParseGuid(s string) (Guid, error) { return xbin.ParseGuid(s) }

// GuidFromBytes 从 16 字节的落盘形态还原，少于 16 字节报错。
func GuidFromBytes(b []byte) (Guid, error) { return xbin.GuidFromBytes(b) }

// TimeMin 是可表示的最早时刻：0001-01-01T00:00:00Z。
func TimeMin() time.Time { return xbin.TimeMin() }

// TimeMax 是可表示的最晚时刻：9999-12-31T23:59:59.9999999Z。
//
// 它带着 100 纳秒的尾巴而不是整毫秒。时间写进文档时会截到毫秒，
// 这两个边界值例外，原样存原样取。
func TimeMax() time.Time { return xbin.TimeMax() }

// ErrInvalidUTF8 表示字符串不是合法 UTF-8，落单的代理项码位也算。
//
// 写入口宁可拒绝也不把非法字节替换成 U+FFFD：替换会让同一份内容两次写出不同的
// 字节，于是索引键不再相等、按原串查不到刚写进去的文档，而全程不报错。
var ErrInvalidUTF8 = xbin.ErrInvalidUTF8

// ErrNullInString 表示字段名里含 0x00。
//
// 字段名靠一个 0x00 终止、前面没有长度前缀，名字自身含 0x00 就等于把它截成两半，
// 后半截会被当成值的字节读，整篇文档从那里开始错位。
var ErrNullInString = xbin.ErrNullInString
