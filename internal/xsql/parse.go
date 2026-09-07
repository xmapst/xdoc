// Package xsql 把一条 SQL 语句解析成 [Statement]。
//
// 只管语句的骨架：哪个集合、哪些子句、各段落的边界在哪。子句里的表达式
// 原样留成文本，由执行层交给 xbexpr 去解析——这里连它们是什么意思都不问。
//
// 投影列表和 UPDATE 的赋值部分是两个例外：它们会被改写成一个文档字面量的文本，
// 好让下一层用同一套求值逻辑处理。
package xsql

import (
	"strconv"
	"strings"

	"github.com/xmapst/xdoc/internal/xbexpr"
)

// Kind 是语句的种类。
type Kind uint8

const (
	KindSelect Kind = iota + 1
	KindInsert
	KindUpdate
	KindDelete
	KindCreateIndex
	KindDropIndex
	KindDropCollection
	KindRenameCollection
	KindPragma
	KindRebuild
	KindCheckpoint
	KindBegin
	KindCommit
	KindRollback
)

// OrderKey 是 ORDER BY 里的一项：排序表达式的文本，加上是不是倒序。
type OrderKey struct {
	Expr string
	Desc bool
}

// Statement 是一条解析好的语句。
//
// 哪些字段有意义取决于 Kind。表达式一律以**文本**形式留在这里，
// 由执行层自己去解析——语句结构和表达式求值是两层事。
type Statement struct {
	// Collection 是语句作用的集合名。
	Collection string
	// Select 是投影表达式的文本。
	//
	// 多个字段会被拼成一个文档字面量；单独的 * 变成 "$"，也就是整篇文档。
	Select string
	// Into 是 SELECT ... INTO 的目标集合，或者 RENAME 的新名字。
	Into string
	// IntoAuto 是 INTO 目标集合的自增主键类型。
	IntoAuto string
	// GroupBy 是分组表达式的文本。
	GroupBy string
	// Having 是分组过滤条件的文本。
	Having string
	// AutoID 是 INSERT 时主键的自动生成方式：GUID、INT、LONG 或 OBJECTID。
	AutoID string
	// Transform 是 UPDATE 的赋值部分，已经拼成一个文档字面量的文本。
	//
	// 写成 SET a = 1, b = 2 也好，直接写一个文档字面量也好，到这里都是同一种形态。
	Transform string
	// IndexName 是索引名。
	IndexName string
	// IndexExpr 是索引表达式的文本。
	IndexExpr string
	// PragmaName 是 PRAGMA 的名字。
	PragmaName string
	// PragmaValue 是 PRAGMA 要设的值；只读时为空。
	PragmaValue string
	// Options 是 REBUILD 的选项，一段 JSON 文本。
	Options string
	// Includes 是 INCLUDE 子句里各个表达式的文本。
	Includes []string
	// Where 是过滤条件的文本。语法上最多写一个，用切片是为了让执行层能再往里追加。
	Where []string
	// OrderBy 是排序键，按书写次序。
	OrderBy []OrderKey
	// Docs 是 INSERT 要插入的各篇文档，JSON 文本。
	Docs []string
	// Limit 是取几条，是否写了看 HasLimit。
	Limit int
	// Offset 是跳过几条，是否写了看 HasOffset。
	Offset int
	// Kind 是语句种类，决定上面哪些字段有意义。
	Kind Kind
	// Explain 表示只要执行计划，不要结果。
	Explain bool
	// ForUpdate 表示查询要拿写锁。
	ForUpdate bool
	// HasLimit 表示写了 LIMIT——0 与「没写」不是一回事。
	HasLimit bool
	// HasOffset 表示写了 OFFSET。
	HasOffset bool
	// NoFrom 表示查询没有 FROM，只是算一个表达式。
	NoFrom bool
	// Unique 表示建的是唯一索引。
	Unique bool
}

// Parse 解析一条语句，按开头的关键字分派。
func Parse(sql string) (*Statement, error) {
	s := &scanner{src: sql}
	switch s.peekWord() {
	case "SELECT", "EXPLAIN":
		return s.parseSelect()
	case "INSERT":
		return s.parseInsert()
	case "UPDATE":
		return s.parseUpdate()
	case "DELETE":
		return s.parseDelete()
	case "CREATE":
		return s.parseCreate()
	case "DROP":
		return s.parseDrop()
	case "RENAME":
		return s.parseRename()
	case "PRAGMA":
		return s.parsePragma()
	case "REBUILD":
		return s.parseRebuild()
	case "CHECKPOINT":
		return s.parseWord(KindCheckpoint, "CHECKPOINT")
	case "BEGIN":
		return s.parseTrans(KindBegin, "BEGIN")
	case "COMMIT":
		return s.parseTrans(KindCommit, "COMMIT")
	case "ROLLBACK":
		return s.parseTrans(KindRollback, "ROLLBACK")
	}
	return nil, s.errf("unknown statement")
}

// parseSelect 解析 SELECT。
//
// 投影之后立刻到头的，是不带 FROM 的表达式查询。其余子句按 INTO、FROM、
// INCLUDE、WHERE、GROUP BY、HAVING、ORDER BY、LIMIT、OFFSET、FOR UPDATE
// 的固定次序读，每一段都可以省略，但**次序不能换**。
func (s *scanner) parseSelect() (*Statement, error) {
	st := &Statement{Kind: KindSelect}
	if s.accept("EXPLAIN") {
		st.Explain = true
	}
	if err := s.expect("SELECT"); err != nil {
		return nil, err
	}
	sel, err := s.selectList()
	if err != nil {
		return nil, err
	}
	st.Select = sel

	if s.eof() {
		st.NoFrom = true
		return st, s.end()
	}

	if s.accept("INTO") {
		if st.Into, err = s.collection("collection name"); err != nil {
			return nil, err
		}
		if st.IntoAuto, err = s.parseAutoID(); err != nil {
			return nil, err
		}
	}
	if err := s.expect("FROM"); err != nil {
		return nil, err
	}
	if st.Collection, err = s.collection("collection name"); err != nil {
		return nil, err
	}

	for s.accept("INCLUDE") {
		for {
			inc, _, err := s.exprText()
			if err != nil {
				return nil, err
			}
			st.Includes = append(st.Includes, inc)
			if !s.char(',') {
				break
			}
		}
	}
	if s.accept("WHERE") {
		w, _, err := s.exprText()
		if err != nil {
			return nil, err
		}
		st.Where = append(st.Where, w)
	}
	if s.accept("GROUP") {
		if err := s.expect("BY"); err != nil {
			return nil, err
		}
		if st.GroupBy, _, err = s.exprText(); err != nil {
			return nil, err
		}
	}
	if s.accept("HAVING") {
		if st.Having, _, err = s.exprText(); err != nil {
			return nil, err
		}
	}
	if s.accept("ORDER") {
		if err := s.expect("BY"); err != nil {
			return nil, err
		}

		for {
			var k OrderKey
			if k.Expr, _, err = s.exprText(); err != nil {
				return nil, err
			}
			if dir, ok := s.acceptAny("ASC", "DESC"); ok {
				k.Desc = dir == "DESC"
			}
			st.OrderBy = append(st.OrderBy, k)
			if !s.char(',') {
				break
			}
		}
	}
	if s.accept("LIMIT") {
		if st.Limit, err = s.intValue(); err != nil {
			return nil, err
		}
		st.HasLimit = true
	}
	if s.accept("OFFSET") {
		if st.Offset, err = s.intValue(); err != nil {
			return nil, err
		}
		st.HasOffset = true
	}
	if s.accept("FOR") {
		if err := s.expect("UPDATE"); err != nil {
			return nil, err
		}
		st.ForUpdate = true
	}
	return st, s.end()
}

// parseInsert 解析 INSERT，VALUES 后面可以跟多篇文档。
func (s *scanner) parseInsert() (*Statement, error) {
	st := &Statement{Kind: KindInsert}
	if err := s.expect("INSERT"); err != nil {
		return nil, err
	}
	if err := s.expect("INTO"); err != nil {
		return nil, err
	}
	var err error
	if st.Collection, err = s.name("collection name"); err != nil {
		return nil, err
	}
	if st.AutoID, err = s.parseAutoID(); err != nil {
		return nil, err
	}
	if err := s.expect("VALUES"); err != nil {
		return nil, err
	}
	for {
		d, err := s.jsonText()
		if err != nil {
			return nil, err
		}
		st.Docs = append(st.Docs, d)
		if !s.char(',') {
			break
		}
	}
	return st, s.end()
}

// parseAutoID 解析集合名后面的 :类型，用来指定主键的自动生成方式。没写冒号就返回空串。
func (s *scanner) parseAutoID() (string, error) {
	if !s.char(':') {
		return "", nil
	}
	w, ok := s.acceptAny("GUID", "INT", "LONG", "OBJECTID")
	if !ok {
		return "", s.errf("expected GUID, INT, LONG or OBJECTID")
	}
	return w, nil
}

// parseUpdate 解析 UPDATE。
func (s *scanner) parseUpdate() (*Statement, error) {
	st := &Statement{Kind: KindUpdate}
	if err := s.expect("UPDATE"); err != nil {
		return nil, err
	}
	var err error
	if st.Collection, err = s.name("collection name"); err != nil {
		return nil, err
	}
	if err := s.expect("SET"); err != nil {
		return nil, err
	}
	if st.Transform, err = s.parseSet(); err != nil {
		return nil, err
	}
	if s.accept("WHERE") {
		w, _, err := s.exprText()
		if err != nil {
			return nil, err
		}
		st.Where = append(st.Where, w)
	}
	return st, s.end()
}

// parseSet 解析 SET 部分，统一成一个文档字面量的文本。
//
// 直接写文档字面量的原样返回；写成键值对的就拼成文档，键按字符串字面量转义。
func (s *scanner) parseSet() (string, error) {
	s.skipSpace()
	if s.pos < len(s.src) && s.src[s.pos] == '{' {
		t, _, err := s.exprText()
		return t, err
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; ; i++ {
		key, err := s.setKey()
		if err != nil {
			return "", err
		}
		if !s.char('=') {
			return "", s.errf("expected = after %q", key)
		}
		val, _, err := s.exprText()
		if err != nil {
			return "", err
		}
		if i > 0 {
			b.WriteByte(',')
		}

		b.WriteString(strconv.Quote(key))
		b.WriteByte(':')
		b.WriteString(val)
		if !s.char(',') {
			break
		}
	}
	b.WriteByte('}')
	return b.String(), nil
}

// setKey 读一个赋值目标的键：带引号的串、纯数字，或者一个标识符。
func (s *scanner) setKey() (string, error) {
	s.skipSpace()
	if s.pos < len(s.src) {
		if q := s.src[s.pos]; q == '\'' || q == '"' {
			return s.quoted(q)
		}
		if c := s.src[s.pos]; c >= '0' && c <= '9' {
			start := s.pos
			for s.pos < len(s.src) && s.src[s.pos] >= '0' && s.src[s.pos] <= '9' {
				s.pos++
			}
			return s.src[start:s.pos], nil
		}
	}
	return s.name("field name")
}

// quoted 读一个带引号的串并解转义。
//
// 只认 n、t、r 三个转义字母，其余反斜杠后面的字符原样取用。没有收尾引号就报错。
func (s *scanner) quoted(q byte) (string, error) {
	s.pos++
	var b strings.Builder
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch c {
		case q:
			s.pos++
			return b.String(), nil
		case '\\':
			s.pos++
			if s.pos >= len(s.src) {
				return "", s.errf("unterminated string")
			}
			switch e := s.src[s.pos]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(e)
			}
			s.pos++
		default:
			b.WriteByte(c)
			s.pos++
		}
	}
	return "", s.errf("unterminated string")
}

// parseDelete 解析 DELETE。
func (s *scanner) parseDelete() (*Statement, error) {
	st := &Statement{Kind: KindDelete}
	if err := s.expect("DELETE"); err != nil {
		return nil, err
	}
	var err error
	if st.Collection, err = s.name("collection name"); err != nil {
		return nil, err
	}
	if s.accept("WHERE") {
		w, _, err := s.exprText()
		if err != nil {
			return nil, err
		}
		st.Where = append(st.Where, w)
	}
	return st, s.end()
}

// parseCreate 解析 CREATE INDEX，索引表达式写在一对括号里。
func (s *scanner) parseCreate() (*Statement, error) {
	st := &Statement{Kind: KindCreateIndex}
	if err := s.expect("CREATE"); err != nil {
		return nil, err
	}
	st.Unique = s.accept("UNIQUE")
	if err := s.expect("INDEX"); err != nil {
		return nil, err
	}
	var err error
	if st.IndexName, err = s.name("index name"); err != nil {
		return nil, err
	}
	if err := s.expect("ON"); err != nil {
		return nil, err
	}
	if st.Collection, err = s.name("collection name"); err != nil {
		return nil, err
	}
	if !s.char('(') {
		return nil, s.errf("expected ( before index expression")
	}
	if st.IndexExpr, _, err = s.exprText(); err != nil {
		return nil, err
	}
	if !s.char(')') {
		return nil, s.errf("expected ) after index expression")
	}
	return st, s.end()
}

// parseDrop 解析 DROP COLLECTION 或 DROP INDEX。索引要写成「集合名.索引名」。
func (s *scanner) parseDrop() (*Statement, error) {
	if err := s.expect("DROP"); err != nil {
		return nil, err
	}
	switch {
	case s.accept("COLLECTION"):
		st := &Statement{Kind: KindDropCollection}
		var err error
		st.Collection, err = s.name("collection name")
		if err != nil {
			return nil, err
		}
		return st, s.end()
	case s.accept("INDEX"):
		st := &Statement{Kind: KindDropIndex}
		var err error
		if st.Collection, err = s.name("collection name"); err != nil {
			return nil, err
		}
		if !s.char('.') {
			return nil, s.errf("expected . between collection and index name")
		}
		if st.IndexName, err = s.name("index name"); err != nil {
			return nil, err
		}
		return st, s.end()
	}
	return nil, s.errf("expected COLLECTION or INDEX")
}

// parseRename 解析 RENAME COLLECTION。
func (s *scanner) parseRename() (*Statement, error) {
	st := &Statement{Kind: KindRenameCollection}
	if err := s.expect("RENAME"); err != nil {
		return nil, err
	}
	if err := s.expect("COLLECTION"); err != nil {
		return nil, err
	}
	var err error
	if st.Collection, err = s.name("collection name"); err != nil {
		return nil, err
	}
	if err := s.expect("TO"); err != nil {
		return nil, err
	}
	if st.Into, err = s.name("new collection name"); err != nil {
		return nil, err
	}
	return st, s.end()
}

// parsePragma 解析 PRAGMA。不带等号就是读，带了就是写。
func (s *scanner) parsePragma() (*Statement, error) {
	st := &Statement{Kind: KindPragma}
	if err := s.expect("PRAGMA"); err != nil {
		return nil, err
	}
	var err error
	if st.PragmaName, err = s.name("pragma name"); err != nil {
		return nil, err
	}
	if s.char('=') {
		if st.PragmaValue, err = s.jsonText(); err != nil {
			return nil, err
		}
	}
	return st, s.end()
}

// parseRebuild 解析 REBUILD，后面可以跟一段 JSON 选项。
func (s *scanner) parseRebuild() (*Statement, error) {
	st := &Statement{Kind: KindRebuild}
	if err := s.expect("REBUILD"); err != nil {
		return nil, err
	}
	if !s.eof() {
		var err error
		if st.Options, err = s.jsonText(); err != nil {
			return nil, err
		}
	}
	return st, s.end()
}

// parseWord 解析只有一个关键字的语句。
func (s *scanner) parseWord(k Kind, kw string) (*Statement, error) {
	if err := s.expect(kw); err != nil {
		return nil, err
	}
	return &Statement{Kind: k}, s.end()
}

// parseTrans 解析事务语句，后面可以缀上可有可无的 TRANS 或 TRANSACTION。
func (s *scanner) parseTrans(k Kind, kw string) (*Statement, error) {
	if err := s.expect(kw); err != nil {
		return nil, err
	}
	s.acceptAny("TRANS", "TRANSACTION")
	return &Statement{Kind: k}, s.end()
}

// intValue 读一个非负整数，读不到或超出范围就报错。
func (s *scanner) intValue() (int, error) {
	s.skipSpace()
	start := s.pos
	for s.pos < len(s.src) && s.src[s.pos] >= '0' && s.src[s.pos] <= '9' {
		s.pos++
	}
	if start == s.pos {
		return 0, s.errf("expected an integer")
	}
	n, err := strconv.Atoi(s.src[start:s.pos])
	if err != nil {
		return 0, s.errf("integer out of range")
	}
	return n, nil
}

// selectList 解析投影列表，拼成一个表达式文本。
//
// 每一列都要有名字：写了 AS 就用别名，没写就按表达式引用到的字段名推一个。
// 重名的自动缀上序号，从 1 起。
//
// 三种情形不拼文档，直接用原样：单独一个 $（整篇文档，产出 "$"）、
// 单独一个文档字面量、单独一次 EXTEND 调用——它们本身就已经是一篇文档了。
func (s *scanner) selectList() (string, error) {
	type field struct{ key, src string }
	var (
		fields  []field
		firstN  xbexpr.Node
		names   = map[string]bool{}
		counter = 1
	)
	add := func(alias string, n xbexpr.Node, src string) {
		if names[alias] {
			alias += strconv.Itoa(counter)
			counter++
		}
		names[alias] = true

		if len(fields) == 0 {
			firstN = n
		}
		fields = append(fields, field{alias, src})
	}

	for {
		src, n, err := s.exprText()
		if err != nil {
			return "", err
		}
		if s.atSelectEnd() {
			add(xbexpr.DefaultFieldName(n), n, src)
			break
		}
		if s.char(',') {
			add(xbexpr.DefaultFieldName(n), n, src)
			continue
		}
		s.accept("AS")
		alias, err := s.name("a column alias")
		if err != nil {
			return "", err
		}
		add(alias, n, src)
		if s.atSelectEnd() {
			break
		}
		if !s.char(',') {
			return "", s.errf("expected , or FROM after a column alias")
		}
	}

	if len(fields) == 1 {
		switch t := firstN.(type) {
		case *xbexpr.PathNode:
			if t != nil && t.Root == xbexpr.RootDocument && len(t.Steps) == 0 {
				return "$", nil
			}
		case *xbexpr.DocumentNode:
			return fields[0].src, nil
		case *xbexpr.CallNode:
			if t != nil && strings.EqualFold(t.Name, "EXTEND") {
				return fields[0].src, nil
			}
		}
	}

	var b strings.Builder
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}

		b.WriteString(strconv.Quote(f.key))
		b.WriteByte(':')
		b.WriteString(f.src)
	}
	b.WriteByte('}')
	return b.String(), nil
}

// atSelectEnd 判断投影列表是不是到头了：走到末尾，或者下一个词是 FROM 或 INTO。
func (s *scanner) atSelectEnd() bool {
	if s.eof() {
		return true
	}
	w := s.peekWord()
	return w == "FROM" || w == "INTO"
}
