package xjson

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/xmapst/xdoc/internal/xbin"
)

// maxDecimalScale 是十进制数最多几位小数。
const maxDecimalScale = 28

// decimalLimit 是 2^96，十进制数的尾数必须小于它。
var decimalLimit = new(big.Int).Lsh(big.NewInt(1), 96)

// parseDecimal 把一个数字串解析成十进制定点数。
//
// 不接受科学计数、不接受空的数字部分。小数位数就是小数点后的字符个数——
// "1.50" 与 "1.5" 解出来是两个不同的值，那个差别要保住。
//
// 尾数用大整数中转，再拆成三段 32 位：96 位放不下大整数，
// 所以用 [big.Int.FillBytes] 定长填充之后按段取。
func parseDecimal(s string) (xbin.Decimal, error) {
	t := strings.TrimSpace(s)
	neg := false
	if len(t) > 0 && (t[0] == '+' || t[0] == '-') {
		neg = t[0] == '-'
		t = t[1:]
	}
	intPart, fracPart, hasDot := strings.Cut(t, ".")
	if !isDigits(intPart) || (hasDot && !isDigits(fracPart)) {
		return xbin.Decimal{}, fmt.Errorf("xjson: %q is not a decimal number", s)
	}
	if len(intPart)+len(fracPart) == 0 {
		return xbin.Decimal{}, fmt.Errorf("xjson: %q contains no digits", s)
	}
	if len(fracPart) > maxDecimalScale {
		return xbin.Decimal{}, fmt.Errorf("xjson: %q has %d fraction digits, over the limit of %d",
			s, len(fracPart), maxDecimalScale)
	}
	m, ok := new(big.Int).SetString("0"+intPart+fracPart, 10)
	if !ok {
		return xbin.Decimal{}, fmt.Errorf("xjson: %q is not a decimal number", s)
	}
	if m.Cmp(decimalLimit) >= 0 {
		return xbin.Decimal{}, fmt.Errorf("xjson: significant digits of %q exceed the decimal range", s)
	}

	var buf [12]byte
	m.FillBytes(buf[:])
	return xbin.DecimalFromParts(
		binary.BigEndian.Uint32(buf[8:]),
		binary.BigEndian.Uint32(buf[4:]),
		binary.BigEndian.Uint32(buf[0:]),
		uint8(len(fracPart)), neg)
}

// isDigits 报告一个串是不是全由数字组成，空串算是。
func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseGUID 解析标识文本，认花括号、圆括号包裹与不带连字符的写法。
//
// 统一补成 8-4-4-4-12 再交给底层解析，省得那边也认一遍各种写法。
func parseGUID(s string) (xbin.Guid, error) {
	t := strings.TrimSpace(s)
	if len(t) >= 2 {
		if (t[0] == '{' && t[len(t)-1] == '}') || (t[0] == '(' && t[len(t)-1] == ')') {
			t = t[1 : len(t)-1]
		}
	}
	if len(t) == 32 {
		t = t[0:8] + "-" + t[8:12] + "-" + t[12:16] + "-" + t[16:20] + "-" + t[20:32]
	}
	return xbin.ParseGuid(t)
}

// dateLayouts 是日期串能接受的几种写法，按从严到宽的次序试。
//
// 带时区的排在前面：不带时区的写法会把带时区的串也部分匹配上，
// 先试宽的会丢掉时区。
var dateLayouts = []string{
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parseDate 解析一个 ISO-8601 时刻，结果转成 UTC。
func parseDate(s string) (time.Time, error) {
	t := strings.TrimSpace(s)
	for _, layout := range dateLayouts {
		if v, err := time.Parse(layout, t); err == nil {
			return v.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("xjson: %q is not an ISO-8601 instant", s)
}

// decodeBase64 解码 base64，先去掉里面的空白。
//
// 只在真的含空白时才重建串，省掉一次分配。
func decodeBase64(s string) ([]byte, error) {
	if strings.ContainsAny(s, " \t\r\n") {
		s = strings.Map(func(r rune) rune {
			switch r {
			case ' ', '\t', '\r', '\n':
				return -1
			}
			return r
		}, s)
	}
	return base64.StdEncoding.DecodeString(s)
}
