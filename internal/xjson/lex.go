package xjson

import (
	"bufio"
	"io"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/xmapst/xdoc/internal/xerr"
)

// maxTokenLen 是单个记号的字符数上限。
//
// 有这个上限，一份没有收尾引号的输入就不会把内存吃光。
const maxTokenLen = 1 << 26

// tokKind 是记号种类。
type tokKind uint8

const (
	// 记号种类。tokOther 是认不出来的单个字符，交给解析器去报错。
	tokEOF tokKind = iota
	tokOpenBrace
	tokCloseBrace
	tokOpenBracket
	tokCloseBracket
	tokComma
	tokColon
	tokMinus
	tokString
	tokInt
	tokDouble
	tokWord
	tokSpace

	tokOther
)

// String 返回记号种类的可读名，用在错误消息里。
func (k tokKind) String() string {
	switch k {
	case tokEOF:
		return "end of input"
	case tokOpenBrace:
		return "{"
	case tokCloseBrace:
		return "}"
	case tokOpenBracket:
		return "["
	case tokCloseBracket:
		return "]"
	case tokComma:
		return ","
	case tokColon:
		return ":"
	case tokMinus:
		return "-"
	case tokString:
		return "string"
	case tokInt:
		return "integer"
	case tokDouble:
		return "number"
	case tokWord:
		return "word"
	case tokSpace:
		return "whitespace"
	default:
		return "symbol"
	}
}

// token 是一个记号：种类、文本，以及它在输入里的字符位置。
type token struct {
	kind tokKind

	// text 是记号的文本（字符串已经解过转义），pos 是它的起始字符位置。
	text string
	pos  int64
}

// lexer 从一个流里逐字符切记号。
//
// 走流而不是整段字节：导入一份大 JSON 文件时不必先把它整个读进内存。
type lexer struct {
	// src 是输入流，ch 是当前字符，eof 与 err 是流的状态，pos 是已读字符数。
	src *bufio.Reader
	ch  rune
	eof bool
	err error
	pos int64
}

// newLexer 建一个切词器并读进第一个字符。
func newLexer(r io.Reader) *lexer {
	l := &lexer{src: bufio.NewReader(r)}
	l.readChar()
	return l
}

// readChar 读进下一个字符。
//
// 不合法的 UTF-8 记成错误但**继续往下读**：错误留到下一次取记号时才交出去，
// 这样调用点只需要在一处判错。
func (l *lexer) readChar() {
	if l.eof {
		l.ch = 0
		return
	}
	r, size, err := l.src.ReadRune()
	if err != nil {
		l.ch = 0
		l.eof = true
		if err != io.EOF && l.err == nil {
			l.err = xerr.InvalidFormat.Wrapf(err, "failed to read JSON input")
		}
		return
	}

	if r == utf8.RuneError && size == 1 && l.err == nil {
		l.err = xerr.InvalidFormat.Newf("invalid UTF-8 byte").At(l.pos)
	}
	l.pos++
	l.ch = r
}

// isWordChar 报告一个字符能不能出现在不带引号的键名里。
//
// 首字符不能是数字。辅助平面的字符一律不算：键名里出现它们的情形
// 不存在，而排除掉能让判断只看一个编码单元。
func isWordChar(r rune, first bool) bool {
	if r > 0xFFFF {
		return false
	}
	if r == '_' || r == '$' {
		return true
	}
	if unicode.IsLetter(r) {
		return true
	}
	return !first && unicode.IsDigit(r)
}

// readToken 取下一个记号。
//
// 比标准 JSON 宽：单引号也能引字符串，键名可以不加引号，
// `--` 到行尾算注释。这些宽松写法是格式的一部分。
func (l *lexer) readToken(eatSpace bool) (token, error) {
	for {
		if eatSpace {
			for !l.eof && unicode.IsSpace(l.ch) {
				l.readChar()
			}
		}
		if l.err != nil {
			return token{}, l.err
		}
		if l.eof {
			return token{kind: tokEOF, pos: l.pos}, nil
		}

		c := l.ch
		start := l.pos
		switch c {
		case '{':
			l.readChar()
			return token{tokOpenBrace, "{", start}, nil
		case '}':
			l.readChar()
			return token{tokCloseBrace, "}", start}, nil
		case '[':
			l.readChar()
			return token{tokOpenBracket, "[", start}, nil
		case ']':
			l.readChar()
			return token{tokCloseBracket, "]", start}, nil
		case ',':
			l.readChar()
			return token{tokComma, ",", start}, nil
		case ':':
			l.readChar()
			return token{tokColon, ":", start}, nil
		case '$':
			l.readChar()
			if isWordChar(l.ch, true) {
				w, err := l.readWord()
				if err != nil {
					return token{}, err
				}
				return token{tokWord, "$" + w, start}, nil
			}
			return token{tokOther, "$", start}, nil
		case '-':
			l.readChar()
			if l.eof || l.ch != '-' {
				return token{tokMinus, "-", start}, nil
			}

			l.skipLine()
			continue
		case '"', '\'':
			s, err := l.readString(c)
			if err != nil {
				return token{}, err
			}
			return token{tokString, s, start}, nil
		}

		if c >= '0' && c <= '9' {
			text, isDouble, err := l.readNumber()
			if err != nil {
				return token{}, err
			}
			if isDouble {
				return token{tokDouble, text, start}, nil
			}
			return token{tokInt, text, start}, nil
		}
		if unicode.IsSpace(c) {
			for !l.eof && unicode.IsSpace(l.ch) {
				l.readChar()
			}
			return token{tokSpace, " ", start}, nil
		}
		if isWordChar(c, true) {
			w, err := l.readWord()
			if err != nil {
				return token{}, err
			}
			return token{tokWord, w, start}, nil
		}
		l.readChar()
		return token{tokOther, string(c), start}, nil
	}
}

// skipLine 跳到行尾，用来吃掉注释。
func (l *lexer) skipLine() {
	for !l.eof && l.ch != '\n' {
		l.readChar()
	}
	if l.ch == '\n' {
		l.readChar()
	}
}

// readWord 读出一个不带引号的词。
func (l *lexer) readWord() (string, error) {
	var b strings.Builder
	b.WriteRune(l.ch)
	l.readChar()
	for !l.eof && isWordChar(l.ch, false) {
		if b.Len() >= maxTokenLen {
			return "", l.tooLong("key name")
		}
		b.WriteRune(l.ch)
		l.readChar()
	}
	return b.String(), l.err
}

// readNumber 读出一个数字，第二个返回值说明它带不带小数点或指数。
//
// 小数点与指数各只许出现一次，正负号只许紧跟在指数标记之后。
// 不合规的字符就地停下，让解析器去处理剩下的部分——切词器不判断
// 「这是不是一个合法的数」。
func (l *lexer) readNumber() (string, bool, error) {
	var b strings.Builder
	b.WriteRune(l.ch)
	isDouble := false
	canDot, canExp, canSign := true, true, false
	l.readChar()
	for !l.eof {
		c := l.ch
		switch {
		case c == '.':
			if !canDot {
				return b.String(), isDouble, l.err
			}
			isDouble, canDot = true, false
		case c == 'e' || c == 'E':
			if !canExp {
				return b.String(), isDouble, l.err
			}
			isDouble, canExp, canSign = true, false, true
		case c == '+' || c == '-':
			if !canSign {
				return b.String(), isDouble, l.err
			}
			canSign = false
		case unicode.IsDigit(c):
			canSign = false
		default:
			return b.String(), isDouble, l.err
		}
		if b.Len() >= maxTokenLen {
			return "", false, l.tooLong("number")
		}
		b.WriteRune(c)
		l.readChar()
	}
	return b.String(), isDouble, l.err
}

// readString 读出一个带引号的字符串，转义就地解开。
//
// 引号字符自身可以用反斜杠转义，所以单引号串里能写 \'。
func (l *lexer) readString(quote rune) (string, error) {
	var b strings.Builder
	l.readChar()
	for {
		if l.err != nil {
			return "", l.err
		}
		if l.eof {
			return "", xerr.UnexpectedToken.Newf("unterminated string").At(l.pos)
		}
		if b.Len() >= maxTokenLen {
			return "", l.tooLong("string")
		}
		if l.ch == quote {
			l.readChar()
			return b.String(), nil
		}
		if l.ch != '\\' {
			b.WriteRune(l.ch)
			l.readChar()
			continue
		}

		pos := l.pos
		l.readChar()
		if l.eof {
			return "", xerr.UnexpectedToken.Newf("string ends with a backslash").At(pos)
		}
		if l.ch == quote {
			b.WriteRune(quote)
			l.readChar()
			continue
		}
		switch l.ch {
		case '\\':
			b.WriteByte('\\')
		case '/':
			b.WriteByte('/')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			r, err := l.readUnicodeEscape(pos)
			if err != nil {
				return "", err
			}
			b.WriteRune(r)
			continue
		default:
			return "", xerr.UnexpectedToken.Newf("unknown escape \\%s", string(l.ch)).At(pos)
		}
		l.readChar()
	}
}

// readUnicodeEscape 读出 \uXXXX，代理对要配对读两个。
//
// 落单的代理项报错而不是写成替换字符：那会把一个可恢复的输入错误
// 变成一段悄悄损坏的文本。
func (l *lexer) readUnicodeEscape(pos int64) (rune, error) {
	cu, err := l.readHex4(pos)
	if err != nil {
		return 0, err
	}
	if !utf16.IsSurrogate(cu) {
		return cu, nil
	}

	bad := xerr.UnexpectedToken.Newf("lone surrogate \\u%04x", cu).At(pos)
	if l.ch != '\\' {
		return 0, bad
	}
	l.readChar()
	if l.ch != 'u' {
		return 0, bad
	}
	lo, err := l.readHex4(pos)
	if err != nil {
		return 0, err
	}
	r := utf16.DecodeRune(cu, lo)
	if r == utf8.RuneError {
		return 0, bad
	}
	return r, nil
}

// readHex4 读出四个十六进制字符。
func (l *lexer) readHex4(pos int64) (rune, error) {
	var v rune
	for range 4 {
		l.readChar()
		if l.err != nil {
			return 0, l.err
		}
		if l.eof {
			return 0, xerr.UnexpectedToken.Newf("fewer than four hex digits after \\u").At(pos)
		}
		d := hexVal(l.ch)
		if d < 0 {
			return 0, xerr.UnexpectedToken.Newf("non-hex character %q after \\u", string(l.ch)).At(pos)
		}
		v = v<<4 | rune(d)
	}
	l.readChar()
	return v, nil
}

// hexVal 把一个十六进制字符转成数值，不是的给 -1。
func hexVal(r rune) int {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0')
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10
	case r >= 'A' && r <= 'F':
		return int(r-'A') + 10
	}
	return -1
}

// tooLong 造一个「记号太长」的错误。
func (l *lexer) tooLong(what string) error {
	return xerr.InvalidFormat.Newf("%s longer than %d characters", what, maxTokenLen).At(l.pos)
}
