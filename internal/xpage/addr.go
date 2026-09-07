package xpage

import (
	"encoding/binary"
	"fmt"
)

// ReadAddress 从 5 个字节读出一个地址。
func ReadAddress(b []byte) Address {
	return Address{PageID: binary.LittleEndian.Uint32(b), Index: b[4]}
}

// WriteAddress 把地址写进 5 个字节。
func (a Address) WriteAddress(b []byte) {
	binary.LittleEndian.PutUint32(b, a.PageID)
	b[4] = a.Index
}

// AppendAddress 把地址追加进 dst。
func (a Address) AppendAddress(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, a.PageID)
	return append(dst, a.Index)
}

// String 写成 页号:槽号，空地址写成 (none)。
func (a Address) String() string {
	if a.IsEmpty() {
		return "(none)"
	}
	return fmt.Sprintf("%d:%d", a.PageID, a.Index)
}
