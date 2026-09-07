package xsql

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/xmapst/xdoc/internal/xbexpr"
)

// scanner 在 SQL 文本上往前走，pos 是当前位置。
type scanner struct {
	src string
	pos int
}

// skipSpace 跳过空白和行注释。两个减号起头到行尾算注释。
func (s *scanner) skipSpace() {
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			s.pos++
		case c == '-' && s.pos+1 < len(s.src) && s.src[s.pos+1] == '-':
			for s.pos < len(s.src) && s.src[s.pos] != '\n' {
				s.pos++
			}
		default:
			return
		}
	}
}

// eof 判断有没有走到头。末尾的分号连同它后面的空白都算走到头。
func (s *scanner) eof() bool {
	s.skipSpace()
	if s.pos >= len(s.src) {
		return true
	}
	return s.src[s.pos] == ';' && strings.TrimSpace(s.src[s.pos+1:]) == ""
}

// peekWord 看下一个词但不吃掉，转成大写返回。
func (s *scanner) peekWord() string {
	save := s.pos
	w := s.readWord()
	s.pos = save
	return strings.ToUpper(w)
}

// readWord 读一个标识符并吃掉它。
//
// 首字符可以是字母、下划线或美元号，后续再加数字。读不到就返回空串。
func (s *scanner) readWord() string {
	s.skipSpace()
	start := s.pos
	for s.pos < len(s.src) {
		r, size := utf8.DecodeRuneInString(s.src[s.pos:])
		if r == utf8.RuneError && size <= 1 {
			break
		}
		first := s.pos == start
		if r == '_' || r == '$' || unicode.IsLetter(r) || (!first && unicode.IsDigit(r)) {
			s.pos += size
			continue
		}
		break
	}
	return s.src[start:s.pos]
}

// accept 下一个词是 kw 就吃掉并返回真，否则位置不动。比较不分大小写。
func (s *scanner) accept(kw string) bool {
	save := s.pos
	if strings.EqualFold(s.readWord(), kw) {
		return true
	}
	s.pos = save
	return false
}

// acceptAny 同 [scanner.accept]，但试一组词，命中时返回它的大写形式。
func (s *scanner) acceptAny(kws ...string) (string, bool) {
	save := s.pos
	w := s.readWord()
	for _, kw := range kws {
		if strings.EqualFold(w, kw) {
			return strings.ToUpper(w), true
		}
	}
	s.pos = save
	return "", false
}

// expect 要求下一个词是 kw，不是就报错。
func (s *scanner) expect(kw string) error {
	if s.accept(kw) {
		return nil
	}
	return s.errf("expected %s", kw)
}

// name 读一个标识符，读不到就报错，what 用来说明这里该出现什么。
func (s *scanner) name(what string) (string, error) {
	w := s.readWord()
	if w == "" {
		return "", s.errf("expected %s", what)
	}
	return w, nil
}

// collection 读一个集合名。
//
// $ 开头的是虚拟集合，后面可以跟一对括号带参数，此时把整段括号原样接上。
func (s *scanner) collection(what string) (string, error) {
	w, err := s.name(what)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(w, "$") {
		return w, nil
	}

	if s.pos >= len(s.src) || s.src[s.pos] != '(' {
		return w, nil
	}
	args, err := s.balanced()
	if err != nil {
		return "", err
	}
	return w + args, nil
}

// jsonText 读一段 JSON 文本，返回原样的字符串。
//
// 花括号或方括号起头的按配对读到底；否则一路读到逗号、分号或空白为止，
// 引号里的这些字符不算分界。
func (s *scanner) jsonText() (string, error) {
	s.skipSpace()
	if s.pos >= len(s.src) {
		return "", s.errf("expected a JSON value")
	}
	if c := s.src[s.pos]; c == '{' || c == '[' {
		return s.balanced()
	}
	start := s.pos
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		if c == '\'' || c == '"' {
			q := c
			s.pos++
			for s.pos < len(s.src) && s.src[s.pos] != q {
				if s.src[s.pos] == '\\' {
					s.pos++
				}
				s.pos++
			}
			if s.pos >= len(s.src) {
				return "", s.errf("unterminated string")
			}
			s.pos++
			continue
		}
		if c == ',' || c == ';' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			break
		}
		s.pos++
	}
	if s.pos == start {
		return "", s.errf("expected a JSON value")
	}
	return s.src[start:s.pos], nil
}

// balanced 从当前位置读一段括号配对的文本，含首尾那对括号。
//
// 三种括号一起计数。引号里的括号不参与计数，反斜杠转义也认。
func (s *scanner) balanced() (string, error) {
	start := s.pos
	depth := 0
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch c {
		case '\'', '"':
			s.pos++
			for s.pos < len(s.src) && s.src[s.pos] != c {
				if s.src[s.pos] == '\\' {
					s.pos++
				}
				s.pos++
			}
			if s.pos >= len(s.src) {
				return "", s.errf("unterminated string in collection arguments")
			}
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				s.pos++
				return s.src[start:s.pos], nil
			}
			if depth < 0 {
				return "", s.errf("unbalanced brackets in collection arguments")
			}
		}
		s.pos++
	}
	return "", s.errf("missing closing parenthesis in collection arguments")
}

// char 跳过空白后，下一个字节是 c 就吃掉并返回真。
func (s *scanner) char(c byte) bool {
	s.skipSpace()
	if s.pos < len(s.src) && s.src[s.pos] == c {
		s.pos++
		return true
	}
	return false
}

// expr 从当前位置解析一个表达式，位置前进到它的末尾。
func (s *scanner) expr() (xbexpr.Node, error) {
	s.skipSpace()
	n, used, err := xbexpr.ParsePrefix(s.src[s.pos:])
	if err != nil {
		return nil, fmt.Errorf("xsql: at offset %d: %w", s.pos, err)
	}
	s.pos += used
	return n, nil
}

// exprText 解析一个表达式，同时返回它的原始文本。
//
// 留着原文是因为语句里的表达式要原样传给下一层，而不是还原自语法树——
// 原文更贴近用户写的样子。
func (s *scanner) exprText() (string, xbexpr.Node, error) {
	s.skipSpace()
	start := s.pos
	n, err := s.expr()
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSpace(s.src[start:s.pos]), n, nil
}

// errf 造一个带位置和附近文本的错误，附近文本截到 24 字节。
func (s *scanner) errf(format string, args ...any) error {
	near := s.src[min(s.pos, len(s.src)):]
	if len(near) > 24 {
		near = near[:24]
	}
	return fmt.Errorf("xsql: %s at offset %d, near %q",
		fmt.Sprintf(format, args...), s.pos, strings.TrimSpace(near))
}

// end 要求语句到此为止，还有剩余就报错。
func (s *scanner) end() error {
	if s.eof() {
		return nil
	}
	return s.errf("unexpected trailing input")
}
