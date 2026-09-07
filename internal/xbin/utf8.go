package xbin

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrInvalidUTF8 表示串里有不合法的 UTF-8 字节。
var ErrInvalidUTF8 = errors.New("xbin: invalid utf-8")

// ErrNullInString 表示串里含有 0 字节。
var ErrNullInString = errors.New("xbin: null character in string")

// ValidateString 检查串是合法 UTF-8。
//
// 写进文件之前必须过这一关：不合法的字节读回来会变成替换字符，
// 那时原始内容已经找不回来了。
func ValidateString(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: %q", ErrInvalidUTF8, s)
	}
	return nil
}

// ValidateCString 检查串既是合法 UTF-8，又不含 0 字节。
//
// 键名与集合名这类以 0 结尾存放的串走这条：串里再有一个 0，
// 读的时候就会在那里提前截断。
func ValidateCString(s string) error {
	if err := ValidateString(s); err != nil {
		return err
	}
	if strings.IndexByte(s, 0) >= 0 {
		return fmt.Errorf("%w: %q", ErrNullInString, s)
	}
	return nil
}
