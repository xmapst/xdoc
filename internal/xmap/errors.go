package xmap

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrUnsupportedType 表示这个 Go 类型没有对应的文档表示。
	ErrUnsupportedType = errors.New("xmap: unsupported type")

	// ErrMaxDepth 表示嵌套超过了 [Mapper] 的深度上限。
	ErrMaxDepth = errors.New("xmap: max nesting depth exceeded")

	// ErrTarget 表示解码目标不可写：不是指针、是空指针，或者是未导出的字段。
	ErrTarget = errors.New("xmap: target is not writable")

	// ErrTypeMismatch 表示文档里的值与目标类型对不上。
	ErrTypeMismatch = errors.New("xmap: document value does not match target type")

	// ErrOverflow 表示数值装不进目标类型，或者数组/二进制装不进定长目标。
	ErrOverflow = errors.New("xmap: numeric value out of range for target type")

	// ErrNoPrimaryKey 表示这个结构体没有主键字段。
	ErrNoPrimaryKey = errors.New("xmap: type has no primary key field")

	// ErrInvalidTag 表示 bson 标签写得不对。
	ErrInvalidTag = errors.New("xmap: malformed struct tag")

	// ErrDuplicateField 表示两个字段抢同一个文档字段名，且分不出胜负。只在开了严格模式时报。
	ErrDuplicateField = errors.New("xmap: duplicate document field name")

	// ErrPanic 表示编解码过程中发生了 panic，已被拦下转成错误。
	ErrPanic = errors.New("xmap: internal panic")
)

// Error 是一次映射失败，带上出错的位置。
type Error struct {
	// Path 是出错处在文档里的路径，比如 items.[2].name。
	Path string

	// GoType 是出错处对应的 Go 类型。
	GoType string

	// Err 是具体的原因，通常包着本包的某个哨兵错误。
	Err error
}

// Error 把原因、路径和类型拼成一句话。
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Err.Error())
	if e.Path != "" {
		fmt.Fprintf(&b, " (at %s", e.Path)
		if e.GoType != "" {
			fmt.Fprintf(&b, ", type %s", e.GoType)
		}
		b.WriteByte(')')
	} else if e.GoType != "" {
		fmt.Fprintf(&b, " (type %s)", e.GoType)
	}
	return b.String()
}

// Unwrap 返回具体原因，供 [errors.Is] 判别哨兵。
func (e *Error) Unwrap() error { return e.Err }

// errTarget 造一个目标不可写的错误。
func errTarget(msg string) error { return fmt.Errorf("%w: %s", ErrTarget, msg) }

// errUnsupported 造一个类型不支持的错误。
func errUnsupported(msg string) error { return fmt.Errorf("%w: %s", ErrUnsupportedType, msg) }
