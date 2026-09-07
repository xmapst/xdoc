package xlock

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// Strategy 决定怎么从库文件路径算出锁名。
//
// 锁名必须在参与竞争的各方之间一致：算法不同的两个进程会各锁各的，
// 互斥就不成立了。
type Strategy int

const (
	// StrategyDefault 用转义后的路径，太长时（只在 Windows 上会碰到）自动退到摘要。
	StrategyDefault Strategy = iota

	// StrategyUriEscape 一律用转义后的路径。
	StrategyUriEscape

	// StrategySha1 一律用路径的摘要，名字长度固定。
	StrategySha1
)

// Name 算出这份库的锁名。
//
// 路径先归一：转成绝对路径再转小写，这样同一份文件的不同写法算出同一个名字。
func (s Strategy) Name(path string) (string, error) {
	norm, err := normalize(path)
	if err != nil {
		return "", err
	}
	switch s {
	case StrategyDefault, StrategyUriEscape:
		uri := escapeData(norm)

		if runtime.GOOS == "windows" && len(uri)+conservativePrefixLen > windowsNameMax {
			return "sha1-" + sha1Hex(norm), nil
		}
		return uri, nil
	case StrategySha1:
		return sha1Hex(norm), nil
	}
	return "", fmt.Errorf("xlock: unknown strategy %d", int(s))
}

const (
	// windowsNameMax 是内核对象名的长度上限，留了一点余量。
	windowsNameMax = 250

	// conservativePrefixLen 是全局前缀与后缀要占掉的字符数。
	conservativePrefixLen = 13
)

// sha1Hex 取摘要的大写十六进制。
//
// 只用来给路径起一个短而稳定的名字，不承担任何安全用途。
func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// normalize 把路径转成绝对路径并转小写。
//
// 转小写会让区分大小写的文件系统上两份不同的文件算出同一个锁名——
// 那只是多一次互斥，不会出错；反过来漏掉互斥才是真问题。
func normalize(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("xlock: resolve %q: %w", path, err)
	}
	return strings.ToLower(abs), nil
}

// escapeData 把路径转义成只含无保留字符的形式。
//
// 分隔符、冒号、空格这些都不能出现在锁名里。
func escapeData(s string) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		c := s[i]
		if unreserved(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperhex[c>>4])
		b.WriteByte(upperhex[c&0x0f])
	}
	return b.String()
}

// unreserved 报告一个字节能不能原样留在锁名里。
func unreserved(c byte) bool {
	switch {
	case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		return true
	}
	return c == '-' || c == '.' || c == '_' || c == '~'
}

// fullName 给锁名加上全局前缀与后缀，这是它在系统里的完整名字。
func fullName(name string) string { return `Global\` + name + ".Mutex" }
