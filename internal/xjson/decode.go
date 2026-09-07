// Package xjson 是文档值与 JSON 文本之间的互转。
//
// 读的一侧比标准 JSON 宽：单引号、不加引号的键名、可选的逗号、`--` 注释都认。
// 写的一侧则严格产出合法 JSON。
//
// JSON 只有几种类型，装不下文档模型里的全部：64 位整数、十进制数、二进制、
// 标识、日期、两个哨兵值都用 `{"$xxx": "..."}` 这样的扩展写法表达，
// 读回来能还原。向量是例外——它写成普通数组，读回来就是数组。
package xjson

import (
	"bytes"
	"io"
	"iter"
	"strconv"
	"strings"
	"time"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xerr"
)

// Unmarshal 解出开头的一个值，**后面剩下什么都不管**。
//
// 空输入给 [xbson.Null]。要求整段用完的话，用 [UnmarshalExact]。
func Unmarshal(b []byte) (*xbson.Value, error) {
	p := &parser{lx: newLexer(bytes.NewReader(b))}
	return p.one()
}

// UnmarshalExact 解出一个值，并要求它之后就是输入末尾。
//
// 外部传进来的 JSON 走这条：`{"a":1} garbage` 该报错，
// 而不是静默地只取前半截。
func UnmarshalExact(b []byte) (*xbson.Value, error) {
	p := &parser{lx: newLexer(bytes.NewReader(b))}
	v, err := p.one()
	if err != nil {
		return nil, err
	}
	rest, err := p.next()
	if err != nil {
		return nil, err
	}
	if rest.kind != tokEOF {
		return nil, rest.unexpected("expected end of input after the value")
	}
	return v, nil
}

// one 解出一个值，输入为空时给 [xbson.Null]。
func (p *parser) one() (*xbson.Value, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	if t.kind == tokEOF {
		return xbson.Null, nil
	}
	return p.parseValue(t)
}

// Reader 从一个流里一个接一个地读出值。
type Reader struct {
	p *parser
}

// NewReader 建一个流式读取器。
func NewReader(r io.Reader) *Reader { return &Reader{p: &parser{lx: newLexer(r)}} }

// Position 返回已经读到输入的第几个字符。
//
// 到末尾时多算一个：这样报出来的位置指向「末尾之后」，
// 与出错时的位置口径一致。
func (d *Reader) Position() int64 {
	if d.p.lx.eof {
		return d.p.lx.pos + 1
	}
	return d.p.lx.pos
}

// Read 读出下一个值，读完返回 [io.EOF]。
func (d *Reader) Read() (*xbson.Value, error) {
	t, err := d.p.next()
	if err != nil {
		return nil, err
	}
	if t.kind == tokEOF {
		return nil, io.EOF
	}
	return d.p.parseValue(t)
}

// Elements 把输入当成一个数组，逐项交出来。
//
// **不把整个数组读进内存**：导入一份大文件时，一次只有一项在手上。
//
// 空输入产出零项；开头不是 `[` 则报错。
func (d *Reader) Elements() iter.Seq2[*xbson.Value, error] {
	return func(yield func(*xbson.Value, error) bool) {
		p := d.p
		t, err := p.next()
		if err != nil {
			yield(nil, err)
			return
		}
		if t.kind == tokEOF {
			return
		}
		if t.kind != tokOpenBracket {
			yield(nil, t.unexpected("expected an array"))
			return
		}
		if err := p.enter(); err != nil {
			yield(nil, err)
			return
		}
		defer p.leave()

		if t, err = p.next(); err != nil {
			yield(nil, err)
			return
		}
		for t.kind != tokCloseBracket {
			if t.kind == tokEOF {
				yield(nil, t.unexpected("unterminated array"))
				return
			}
			v, err := p.parseValue(t)
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(v, nil) {
				return
			}
			if t, err = p.next(); err != nil {
				yield(nil, err)
				return
			}
			if t.kind == tokComma {
				if t, err = p.next(); err != nil {
					yield(nil, err)
					return
				}
			}
		}
	}
}

// parser 是解析器，depth 记着当前嵌套了几层。
type parser struct {
	lx    *lexer
	depth int
}

// next 取下一个记号，跳过空白。
func (p *parser) next() (token, error) { return p.lx.readToken(true) }

// enter 进入一层嵌套，太深就报错。
//
// 有这个上限，一串 `[[[[...` 就不会把调用栈耗光。
func (p *parser) enter() error {
	p.depth++
	if p.depth > maxDepth {
		return xerr.DocumentMaxDepth.Newf("nesting deeper than %d levels", maxDepth).At(p.lx.pos)
	}
	return nil
}

// leave 退出一层嵌套。
func (p *parser) leave() { p.depth-- }

// parseValue 从一个已经取到的记号开始解出一个值。
//
// 负号与数字是两个记号，所以负数要**不跳空白**地取下一个：
// `- 1` 不是一个数。
//
// null、true、false 不区分大小写。
func (p *parser) parseValue(t token) (*xbson.Value, error) {
	switch t.kind {
	case tokString:
		return xbson.String(t.text), nil
	case tokOpenBrace:
		return p.object()
	case tokOpenBracket:
		return p.array()
	case tokMinus:
		n, err := p.lx.readToken(false)
		if err != nil {
			return nil, err
		}
		if n.kind != tokInt && n.kind != tokDouble {
			return nil, n.unexpected("a digit must follow the minus sign")
		}
		return n.kind.parseNumber("-"+n.text, n.pos)
	case tokInt, tokDouble:
		return t.kind.parseNumber(t.text, t.pos)
	case tokWord:
		switch strings.ToLower(t.text) {
		case "null":
			return xbson.Null, nil
		case "true":
			return xbson.True, nil
		case "false":
			return xbson.False, nil
		}
	}
	return nil, t.unexpected("expected a value")
}

// object 解出一个对象。
//
// 键可以不加引号。逗号是可选的：少写一个不报错。
//
// **只有第一个键**才可能是扩展写法（`$oid`、`$date` 这些）；
// 认出来之后整个对象就是那一个值，后面必须紧跟 `}`。
// 限定在第一个键，是为了让普通对象里以 `$` 开头的键仍然是普通键。
func (p *parser) object() (*xbson.Value, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()

	d := xbson.NewDocument()
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	for t.kind != tokCloseBrace {
		if t.kind == tokEOF {
			return nil, t.unexpected("unterminated object")
		}
		if t.kind != tokString && t.kind != tokWord {
			return nil, t.unexpected("expected a key name")
		}
		key := t.text

		if t, err = p.next(); err != nil {
			return nil, err
		}
		if t.kind != tokColon {
			return nil, t.unexpected("expected a colon after the key name")
		}
		if t, err = p.next(); err != nil {
			return nil, err
		}

		if d.Len() == 0 && strings.HasPrefix(key, "$") {
			v, ok, err := t.extended(key)
			if err != nil {
				return nil, err
			}
			if ok {
				end, err := p.next()
				if err != nil {
					return nil, err
				}
				if end.kind != tokCloseBrace {
					return nil, end.unexpected("expected } right after the extended notation")
				}
				return v, nil
			}
		}

		v, err := p.parseValue(t)
		if err != nil {
			return nil, err
		}
		d.Set(key, v)

		if t, err = p.next(); err != nil {
			return nil, err
		}

		if t.kind == tokComma {
			if t, err = p.next(); err != nil {
				return nil, err
			}
		}
	}
	return d.Value(), nil
}

// array 解出一个数组，逗号同样可选。
func (p *parser) array() (*xbson.Value, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()

	a := xbson.NewArray()
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	for t.kind != tokCloseBracket {
		if t.kind == tokEOF {
			return nil, t.unexpected("unterminated array")
		}
		v, err := p.parseValue(t)
		if err != nil {
			return nil, err
		}
		a.Append(v)

		if t, err = p.next(); err != nil {
			return nil, err
		}
		if t.kind == tokComma {
			if t, err = p.next(); err != nil {
				return nil, err
			}
		}
	}
	return a.Value(), nil
}

// parseNumber 把数字记号转成值。
//
// 整数**能放进 32 位就用 32 位**，否则用 64 位——这决定了存进文件的类型，
// 所以 1 与 4294967296 存出来是两种不同的东西。
//
// 浮点溢出不算错：那时得到的是正负无穷，正是想要的结果。
func (k tokKind) parseNumber(text string, pos int64) (*xbson.Value, error) {
	if k == tokInt {
		if n, err := strconv.ParseInt(text, 10, 32); err == nil {
			return xbson.Int32(int32(n)), nil
		}
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, xerr.InvalidFormat.Newf("integer %q is out of int64 range", text).At(pos)
		}
		return xbson.Int64(n), nil
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return xbson.Double(f), nil
		}
		return nil, xerr.InvalidFormat.Newf("%q is not a valid number", text).At(pos)
	}
	return xbson.Double(f), nil
}

// extended 处理 `{"$oid": "..."}` 这类扩展写法。
//
// 第二个返回值说明这个键是不是扩展写法：不是的话交回去当普通键处理，
// 是的话即使载荷不合法也算「已识别」，报错而不是退回去。
//
// `$date` 解出来之后转到**本地时区**：日期值记着时区，
// 而这些文本多半是给人看的。
func (t token) extended(key string) (*xbson.Value, bool, error) {
	fail := func(what string, err error) (*xbson.Value, bool, error) {
		return nil, true, xerr.InvalidFormat.Wrapf(err, "payload %q of %s is not a valid %s", t.text, key, what).At(t.pos)
	}
	switch key {
	case "$binary":
		b, err := decodeBase64(t.text)
		if err != nil {
			return fail("Base64", err)
		}
		return xbson.Binary(b), true, nil
	case "$oid":
		id, err := xbson.ParseObjectID(t.text)
		if err != nil {
			return fail("object id", err)
		}
		return xbson.OID(id), true, nil
	case "$guid":
		g, err := parseGUID(t.text)
		if err != nil {
			return fail("GUID", err)
		}
		return xbson.GUID(g), true, nil
	case "$date":
		inst, err := parseDate(t.text)
		if err != nil {
			return fail("instant", err)
		}

		v, err := xbson.DateTime(inst.In(time.Local))
		if err != nil {
			return fail("instant", err)
		}
		return v, true, nil
	case "$numberLong":
		n, err := strconv.ParseInt(strings.TrimSpace(t.text), 10, 64)
		if err != nil {
			return fail("64-bit integer", err)
		}
		return xbson.Int64(n), true, nil
	case "$numberDecimal":
		d, err := parseDecimal(t.text)
		if err != nil {
			return fail("decimal", err)
		}
		return xbson.Decimal(d), true, nil
	case "$minValue":
		return xbson.MinValue, true, nil
	case "$maxValue":
		return xbson.MaxValue, true, nil
	}
	return nil, false, nil
}

// unexpected 造一个「记号不对」的错误，带上位置与期望。
func (t token) unexpected(want string) error {
	if t.kind == tokEOF {
		return xerr.UnexpectedToken.Newf("unexpected end of input: %s", want).At(t.pos)
	}
	return xerr.UnexpectedToken.Newf("unexpected %s %q: %s", t.kind, t.text, want).At(t.pos)
}
