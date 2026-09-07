package xmap

import (
	"reflect"
	"time"

	"github.com/xmapst/xdoc/internal/xbin"
	"github.com/xmapst/xdoc/internal/xbson"
)

var (
	// 这些类型有各自的文档表示，编解码时不按结构体展开。
	typeTime     = reflect.TypeFor[time.Time]()
	typeDecimal  = reflect.TypeFor[xbin.Decimal]()
	typeGuid     = reflect.TypeFor[xbin.Guid]()
	typeObjectID = reflect.TypeFor[xbson.ObjectID]()
	typeValue    = reflect.TypeFor[*xbson.Value]()
	typeDocument = reflect.TypeFor[*xbson.Document]()
	typeArray    = reflect.TypeFor[*xbson.Array]()

	typeDocumentVal = reflect.TypeFor[xbson.Document]()
	typeArrayVal    = reflect.TypeFor[xbson.Array]()
)

// isOpaque 判断一个类型是否整体对应一个文档值，不该拆成字段。
//
// 除了上面那几个内建类型，用 [Mapper.RegisterType] 登记过转换器的也算。
// []byte 的元素类型若是不透明的，切片就不会被当成二进制。
func (m *Mapper) isOpaque(t reflect.Type) bool {
	switch t {
	case typeTime, typeDecimal, typeGuid, typeObjectID,
		typeDocumentVal, typeArrayVal:
		return true
	}
	_, ok := m.types[t]
	return ok
}
