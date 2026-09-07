package xbexpr

import (
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// TokenType 是词法记号的类别。
type TokenType uint8

const (
	TokEOF TokenType = iota
	TokUnknown
	TokWS
	TokWord
	TokInt
	TokDouble
	TokString

	TokOpenBrace
	TokCloseBrace
	TokOpenBracket
	TokCloseBracket
	TokOpenParen
	TokCloseParen
	TokComma
	TokColon
	TokSemiColon
	TokAt
	TokHashtag
	TokTilde
	TokPeriod
	TokAmpersand
	TokDollar
	TokExclamation
	TokNotEquals
	TokEquals
	TokGreater
	TokGreaterEq
	TokLess
	TokLessEq
	TokMinus
	TokPlus
	TokAsterisk
	TokSlash
	TokBackslash
	TokPercent
)

// String 返回类别名；标点类直接返回它的字面写法。
func (t TokenType) String() string {
	switch t {
	case TokEOF:
		return "EOF"
	case TokUnknown:
		return "unknown"
	case TokWS:
		return "whitespace"
	case TokWord:
		return "word"
	case TokInt:
		return "int"
	case TokDouble:
		return "double"
	case TokString:
		return "string"
	default:
	}
	if s, ok := t.literal(); ok {
		return s
	}
	return "?"
}

// literal 返回标点类记号的字面写法；不是标点就返回 false。
func (t TokenType) literal() (string, bool) {
	switch t {
	case TokOpenBrace:
		return "{", true
	case TokCloseBrace:
		return "}", true
	case TokOpenBracket:
		return "[", true
	case TokCloseBracket:
		return "]", true
	case TokOpenParen:
		return "(", true
	case TokCloseParen:
		return ")", true
	case TokComma:
		return ",", true
	case TokColon:
		return ":", true
	case TokSemiColon:
		return ";", true
	case TokAt:
		return "@", true
	case TokHashtag:
		return "#", true
	case TokTilde:
		return "~", true
	case TokPeriod:
		return ".", true
	case TokAmpersand:
		return "&", true
	case TokDollar:
		return "$", true
	case TokExclamation:
		return "!", true
	case TokNotEquals:
		return "!=", true
	case TokEquals:
		return "=", true
	case TokGreater:
		return ">", true
	case TokGreaterEq:
		return ">=", true
	case TokLess:
		return "<", true
	case TokLessEq:
		return "<=", true
	case TokMinus:
		return "-", true
	case TokPlus:
		return "+", true
	case TokAsterisk:
		return "*", true
	case TokSlash:
		return "/", true
	case TokBackslash:
		return `\`, true
	case TokPercent:
		return "%", true
	default:
	}
	return "", false
}

// Token 是一个词法记号。
type Token struct {
	Type TokenType

	// Value 是记号的文本。
	//
	// 字符串记号存的是解转义之后的内容，不含引号；其余记号存原样。
	Value string

	// Pos 是记号首字节在源文本里的下标。
	Pos int
}

// isKeyword 判断这个词是不是当运算符用的关键字，比较时不分大小写。
func (t Token) isKeyword() bool {
	if t.Type != TokWord {
		return false
	}
	return foldEqual(t.Value, "BETWEEN") || foldEqual(t.Value, "LIKE") ||
		foldEqual(t.Value, "IN") || foldEqual(t.Value, "AND") ||
		foldEqual(t.Value, "OR") || foldEqual(t.Value, "VECTOR_SIM")
}

// isOperand 判断这个记号能不能接着往下构成二元运算。
//
// 名字里的 operand 与实际含义相反：它认的是运算符，而不是被运算的东西。
func (t Token) isOperand() bool {
	switch t.Type {
	case TokPercent, TokSlash, TokAsterisk, TokPlus, TokMinus,
		TokEquals, TokGreater, TokGreaterEq, TokLess, TokLessEq, TokNotEquals:
		return true
	case TokWord:
		return t.isKeyword()
	default:
	}
	return false
}

// is 判断这个记号是不是某个词，不分大小写。
func (t Token) is(word string) bool {
	return t.Type == TokWord && foldEqual(t.Value, word)
}

// foldEqual 按 ASCII 大小写不敏感比较两个串。
//
// 只折叠 A-Z，非 ASCII 字节按原样比——表达式里的关键字都是 ASCII。
func foldEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

// lowerASCII 把一个 ASCII 大写字母转小写，其余原样返回。
func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// upperASCII 把串里的 ASCII 小写字母转大写。没有小写字母时原样返回，省掉一次分配。
func upperASCII(s string) string {
	need := false
	for i := range len(s) {
		if s[i] >= 'a' && s[i] <= 'z' {
			need = true
			break
		}
	}
	if !need {
		return s
	}
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// isWordFirst 判断一个字符能不能作标识符的首字符：下划线、美元号，或字母。
//
// 超出基本多文种平面的字符一律不算——那些位置上的字母不作标识符用。
func isWordFirst(r rune) bool {
	if r > 0xFFFF {
		return false
	}
	return r == '_' || r == '$' || unicode.IsLetter(r)
}

// isWordRest 判断一个字符能不能作标识符的后续字符：首字符那些，再加数字。
func isWordRest(r rune) bool {
	if r > 0xFFFF {
		return false
	}
	return r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// isWord 判断整个串是不是一个合法标识符。空串和纯空白都不是。
func isWord(s string) bool {
	if s == "" || strings.TrimFunc(s, unicode.IsSpace) == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !isWordFirst(r) {
				return false
			}
			continue
		}
		if !isWordRest(r) {
			return false
		}
	}
	return true
}

// eofRune 是读到末尾时的哨兵字符，取一个不可能出现的负值。
const eofRune rune = -1

// lexer 按需切出记号，最多向前看一个。
type lexer struct {
	src string
	// pos 是当前字符的起始下标，w 是它的字节数，r 是它本身。
	pos int
	r   rune
	w   int
	// ahead 是已经切出来但还没取走的那个记号。
	ahead *Token
	// cur 是最近一次取走的记号。
	cur Token
}

// newLexer 造一个词法器并读入首字符。
func newLexer(src string) *lexer {
	l := &lexer{src: src}
	l.read()
	return l
}

// read 前进一个字符；到末尾时把当前字符置为 [eofRune]。
func (l *lexer) read() {
	l.pos += l.w
	if l.pos >= len(l.src) {
		l.pos = len(l.src)
		l.w = 0
		l.r = eofRune
		return
	}
	l.r, l.w = utf8.DecodeRuneInString(l.src[l.pos:])
}

// eof 判断是否已到源文本末尾。
func (l *lexer) eof() bool { return l.r == eofRune }

// lookAhead 看下一个记号但不取走。
//
// 已经看过一个空白记号而这次要求跳过空白时，重新切一个顶替——
// 同一个位置在「要空白」和「不要空白」两种口径下看到的记号不同。
func (l *lexer) lookAhead(eatWS bool) Token {
	if l.ahead != nil {
		if eatWS && l.ahead.Type == TokWS {
			t := l.readNext(true)
			l.ahead = &t
		}
		return *l.ahead
	}
	t := l.readNext(eatWS)
	l.ahead = &t
	return t
}

// readToken 取走下一个记号。空白处理同 [lexer.lookAhead]。
func (l *lexer) readToken(eatWS bool) Token {
	if l.ahead == nil {
		l.cur = l.readNext(eatWS)
		return l.cur
	}
	if eatWS && l.ahead.Type == TokWS {
		t := l.readNext(true)
		l.ahead = &t
	}
	l.cur = *l.ahead
	l.ahead = nil
	return l.cur
}

// readNext 从当前位置切出一个记号。
//
// 外层的循环只为注释服务：两个减号起头到行尾算注释，跳过之后从头再切一次。
// 认不出来的字符切成 [TokUnknown] 而不是报错，由语法分析去处置。
func (l *lexer) readNext(eatWS bool) Token {
	for {
		if eatWS {
			l.eatWhitespace()
		}
		start := l.pos
		if l.eof() {
			return Token{Type: TokEOF, Pos: start}
		}
		switch l.r {
		case '{':
			return l.symbol(TokOpenBrace, start)
		case '}':
			return l.symbol(TokCloseBrace, start)
		case '[':
			return l.symbol(TokOpenBracket, start)
		case ']':
			return l.symbol(TokCloseBracket, start)
		case '(':
			return l.symbol(TokOpenParen, start)
		case ')':
			return l.symbol(TokCloseParen, start)
		case ',':
			return l.symbol(TokComma, start)
		case ':':
			return l.symbol(TokColon, start)
		case ';':
			return l.symbol(TokSemiColon, start)
		case '@':
			return l.symbol(TokAt, start)
		case '#':
			return l.symbol(TokHashtag, start)
		case '~':
			return l.symbol(TokTilde, start)
		case '.':
			return l.symbol(TokPeriod, start)
		case '&':
			return l.symbol(TokAmpersand, start)
		case '=':
			return l.symbol(TokEquals, start)
		case '+':
			return l.symbol(TokPlus, start)
		case '*':
			return l.symbol(TokAsterisk, start)
		case '/':
			return l.symbol(TokSlash, start)
		case '\\':
			return l.symbol(TokBackslash, start)
		case '%':
			return l.symbol(TokPercent, start)

		case '$':
			l.read()
			if isWordFirst(l.r) {
				return Token{Type: TokWord, Value: "$" + l.readWord(), Pos: start}
			}
			return Token{Type: TokDollar, Value: "$", Pos: start}

		case '!':
			l.read()
			if l.r == '=' {
				l.read()
				return Token{Type: TokNotEquals, Value: "!=", Pos: start}
			}
			return Token{Type: TokExclamation, Value: "!", Pos: start}

		case '>':
			l.read()
			if l.r == '=' {
				l.read()
				return Token{Type: TokGreaterEq, Value: ">=", Pos: start}
			}
			return Token{Type: TokGreater, Value: ">", Pos: start}

		case '<':
			l.read()
			if l.r == '=' {
				l.read()
				return Token{Type: TokLessEq, Value: "<=", Pos: start}
			}
			return Token{Type: TokLess, Value: "<", Pos: start}

		case '-':
			l.read()
			if l.r == '-' {
				l.skipLine()
				continue
			}
			return Token{Type: TokMinus, Value: "-", Pos: start}

		case '"', '\'':
			return Token{Type: TokString, Value: l.readString(l.r), Pos: start}
		}

		if l.r >= '0' && l.r <= '9' {
			text, dbl := l.readNumber()
			typ := TokInt
			if dbl {
				typ = TokDouble
			}
			return Token{Type: typ, Value: text, Pos: start}
		}
		if unicode.IsSpace(l.r) {
			for unicode.IsSpace(l.r) && !l.eof() {
				l.read()
			}
			return Token{Type: TokWS, Value: l.src[start:l.pos], Pos: start}
		}
		if isWordFirst(l.r) {
			return Token{Type: TokWord, Value: l.readWord(), Pos: start}
		}

		bad := l.src[l.pos : l.pos+l.w]
		l.read()
		return Token{Type: TokUnknown, Value: bad, Pos: start}
	}
}

// symbol 切出一个单字符标点记号。
func (l *lexer) symbol(t TokenType, start int) Token {
	lit, _ := t.literal()
	l.read()
	return Token{Type: t, Value: lit, Pos: start}
}

// eatWhitespace 跳过连续空白。
func (l *lexer) eatWhitespace() {
	for unicode.IsSpace(l.r) && !l.eof() {
		l.read()
	}
}

// skipLine 跳到行尾，连换行一起吃掉。
func (l *lexer) skipLine() {
	for l.r != '\n' && !l.eof() {
		l.read()
	}
	if l.r == '\n' {
		l.read()
	}
}

// readWord 读一个标识符，返回它在源文本里的那一段。
func (l *lexer) readWord() string {
	start := l.pos
	l.read()
	for !l.eof() && isWordRest(l.r) {
		l.read()
	}
	return l.src[start:l.pos]
}

// readNumber 读一个数，第二个返回值说明它是不是浮点数。
//
// 小数点只许出现一次，指数符号也只许一次，正负号只在指数之后紧跟着才认。
// 读到不合规的字符就停在那里，把已经读到的交出去——是不是一个合法的数
// 由后面的解析去判断。
func (l *lexer) readNumber() (string, bool) {
	start := l.pos
	dbl := false
	canDot, canE, canSign := true, true, false
	l.read()
	for !l.eof() {
		c := l.r
		switch {
		case c >= '0' && c <= '9':
		case c == '.':
			if !canDot {
				return l.src[start:l.pos], dbl
			}
			dbl = true
			canDot = false
		case c == 'e' || c == 'E':
			if !canE {
				return l.src[start:l.pos], dbl
			}
			canE = false
			canSign = true
			dbl = true
		case c == '+' || c == '-':
			if !canSign {
				return l.src[start:l.pos], dbl
			}
			canSign = false
		default:
			return l.src[start:l.pos], dbl
		}
		l.read()
	}
	return l.src[start:l.pos], dbl
}

// readString 读一个带引号的字符串，返回解转义之后的内容。
//
// 先攒成 UTF-16 码元再一次性解码，\u 转义因此能正确拼出代理对。
// 认不出的转义序列被整个丢掉：反斜杠和它后面那个字符都不会进结果。
// 没有收尾引号时读到末尾为止，不报错。
func (l *lexer) readString(quote rune) string {
	var units []uint16
	l.read()
	for l.r != quote && !l.eof() {
		if l.r == '\\' {
			l.read()
			switch l.r {
			case quote:
				units = utf16.AppendRune(units, quote)
			case '\\':
				units = append(units, '\\')
			case '/':
				units = append(units, '/')
			case 'b':
				units = append(units, '\b')
			case 'f':
				units = append(units, '\f')
			case 'n':
				units = append(units, '\n')
			case 'r':
				units = append(units, '\r')
			case 't':
				units = append(units, '\t')
			case 'u':
				units = append(units, l.readHex4())
			}

		} else {
			units = utf16.AppendRune(units, l.r)
		}
		l.read()
	}
	l.read()
	return string(utf16.Decode(units))
}

// readHex4 读四位十六进制，凑成一个 UTF-16 码元。
func (l *lexer) readHex4() uint16 {
	var v uint16
	for range 4 {
		l.read()
		v = v<<4 | hexNibble(l.r)
	}
	return v
}

// hexNibble 把一个十六进制字符转成数值；不是十六进制字符时当 0。
func hexNibble(r rune) uint16 {
	switch {
	case r >= '0' && r <= '9':
		return uint16(r - '0')
	case r >= 'a' && r <= 'f':
		return uint16(r-'a') + 10
	case r >= 'A' && r <= 'F':
		return uint16(r-'A') + 10
	}
	return 0
}
