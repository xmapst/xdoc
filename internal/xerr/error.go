package xerr

import (
	"errors"
	"fmt"
	"strconv"
)

// Error 是带错误码的错误。
//
// 它同时把错误码与被包的错误摊在 Unwrap 里，所以 errors.Is 既能按码比，
// 也能穿透到底层原因。
type Error struct {
	// Code 是这个错误的分类。
	Code Code

	// Msg 是具体说明。
	Msg string

	// Pos 是出错位置（文件偏移或表达式下标），-1 表示没有位置。
	Pos int64

	// Err 是被包住的底层错误，可以为 nil。
	Err error
}

// New 用这个码造一个错误。
func (c Code) New(msg string) *Error {
	return &Error{Code: c, Msg: msg, Pos: -1}
}

// Newf 用这个码造一个带格式化说明的错误。
func (c Code) Newf(format string, args ...any) *Error {
	return &Error{Code: c, Msg: fmt.Sprintf(format, args...), Pos: -1}
}

// Wrap 用这个码包住一个底层错误。
func (c Code) Wrap(err error, msg string) *Error {
	return &Error{Code: c, Msg: msg, Pos: -1, Err: err}
}

// Wrapf 用这个码包住一个底层错误，说明部分格式化。
func (c Code) Wrapf(err error, format string, args ...any) *Error {
	return &Error{Code: c, Msg: fmt.Sprintf(format, args...), Pos: -1, Err: err}
}

// At 返回一份带上位置的副本。
//
// 不就地改：同一个错误值可能被多处引用，改它会让别处的位置跟着变。
func (e *Error) At(pos int64) *Error {
	c := *e
	c.Pos = pos
	return &c
}

// Error 拼出「[码 名] 说明 (at 位置): 底层错误」。
func (e *Error) Error() string {
	s := "[" + strconv.Itoa(int(e.Code)) + " " + e.Code.String() + "] " + e.Msg
	if e.Pos >= 0 {
		s += " (at " + strconv.FormatInt(e.Pos, 10) + ")"
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap 同时交出错误码与底层错误。
//
// 两条都要：按码判定与按底层原因判定都得能走通。
func (e *Error) Unwrap() []error {
	if e.Err == nil {
		return []error{e.Code}
	}
	return []error{e.Code, e.Err}
}

// Critical 报告这是不是「文件坏了」那一类，见 [Code.Critical]。
func (e *Error) Critical() bool { return e.Code.Critical() }

// CodeOf 取出一条错误链上的错误码，没有则给 [Unspecified]。
//
// 先找 [Error]，再找裸的 [Code]。
func CodeOf(err error) Code {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	if c, ok := errors.AsType[Code](err); ok {
		return c
	}
	return Unspecified
}
