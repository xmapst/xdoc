package xmap

import (
	"fmt"
	"reflect"
	"strings"
)

// state 是一趟编解码的过程状态：当前深度，以及走到哪个字段了。
type state struct {
	m     *Mapper
	depth int
	path  []string
}

// newState 开一趟新的过程状态。
func (m *Mapper) newState() *state { return &state{m: m} }

// push 进入一层，seg 是字段名或者 [下标]。
func (s *state) push(seg string) { s.path = append(s.path, seg) }

// pop 退出一层。
func (s *state) pop() { s.path = s.path[:len(s.path)-1] }

// pathString 把当前路径拼成文本；下标段直接贴在前一段后面，不加点。
func (s *state) pathString() string {
	var b strings.Builder
	for i, seg := range s.path {
		if i > 0 && !strings.HasPrefix(seg, "[") {
			b.WriteByte('.')
		}
		b.WriteString(seg)
	}
	return b.String()
}

// fail 给错误补上路径与类型。已经是 [Error] 的原样返回——里层的位置更精确。
func (s *state) fail(t reflect.Type, err error) error {
	var typeName string
	if t != nil {
		typeName = t.String()
	}
	if e, ok := err.(*Error); ok {
		return e
	}
	return &Error{Path: s.pathString(), GoType: typeName, Err: err}
}

// failf 造一个包在哨兵错误下的带位置错误。
func (s *state) failf(t reflect.Type, sentinel error, format string, a ...any) error {
	return s.fail(t, fmt.Errorf("%w: %s", sentinel, fmt.Sprintf(format, a...)))
}

// enter 下探一层，返回退出时该调的函数。
//
// 超过深度上限就报错，并且**不占用**这一层——计数在报错前已经退回。
func (s *state) enter(rv reflect.Value) (func(), error) {
	s.depth++
	if s.depth > s.m.maxDepth() {
		s.depth--
		return nil, s.failf(rv.Type(), ErrMaxDepth, "limit is %d levels", s.m.maxDepth())
	}
	return func() { s.depth-- }, nil
}
