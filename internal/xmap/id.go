package xmap

import (
	"reflect"

	"github.com/xmapst/xdoc/internal/xbson"
)

// PrimaryKey 取出一个实体的主键值。没有主键字段就报错。
func (m *Mapper) PrimaryKey(v any) (val *xbson.Value, err error) {
	defer guard(&err)
	rv, _, err := m.entityValue(v)
	if err != nil {
		return nil, err
	}
	return m.idValue(rv, m.newState())
}

// SetPrimaryKey 把主键值写回实体，v 必须是指针。
//
// 插入之后回填自动生成的主键要靠它。
func (m *Mapper) SetPrimaryKey(v any, id *xbson.Value) (err error) {
	defer guard(&err)
	rv, ptr, err := m.entityValue(v)
	if err != nil {
		return err
	}
	if !ptr {
		return &Error{GoType: reflect.TypeOf(v).String(),
			Err: errTarget("writing the primary key back requires a pointer")}
	}
	p, err := m.planFor(rv.Type())
	if err != nil {
		return err
	}
	if p.idIdx < 0 {
		return &Error{GoType: rv.Type().String(), Err: ErrNoPrimaryKey}
	}
	st := m.newState()
	fv, err := p.fields[p.idIdx].index.allocIn(rv)
	if err != nil {
		return st.fail(rv.Type(), err)
	}
	st.push(idFieldName)
	defer st.pop()
	return m.decode(id, fv, st)
}

// entityValue 剥到底下的结构体值，第二个返回值说明它可不可写。
//
// 一路剥指针和接口，中途遇到空指针就报错。主键只对结构体有意义。
func (m *Mapper) entityValue(v any) (reflect.Value, bool, error) {
	if v == nil {
		return reflect.Value{}, false, &Error{Err: errTarget("value is nil")}
	}
	rv := reflect.ValueOf(v)
	writable := false
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return reflect.Value{}, false, &Error{GoType: rv.Type().String(), Err: errTarget("is a nil pointer")}
		}
		writable = rv.Kind() == reflect.Pointer
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return reflect.Value{}, false, &Error{GoType: rv.Type().String(),
			Err: errUnsupported("a primary key is only meaningful on a struct")}
	}
	return rv, writable && rv.CanSet(), nil
}
