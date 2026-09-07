package xcrypt

import (
	"bytes"

	"github.com/xmapst/xdoc/internal/xerr"
	"github.com/xmapst/xdoc/internal/xpage"
)

// 以下三个量与页头保持一致，用来判断解出来的头页是不是一份真的数据文件。
const (
	HeaderMagicOffset   = xpage.MagicOffset
	HeaderVersionOffset = xpage.VersionOffset
	FormatVersion       = xpage.FileVersion
)

// VerifyHeader 检查解密后的头页：魔数与格式版本都对才算数。
//
// 这两项不对时无法区分「口令错」和「文件根本不是数据文件」——解密总能
// 产出字节，只是错口令解出来的是乱码，所以报错文案把两种可能都写上。
func VerifyHeader(page []byte) error {
	if len(page) < HeaderVersionOffset+1 {
		return xerr.InvalidDatabase.Newf("header page too short: %d bytes", len(page))
	}
	if !bytes.Equal(page[HeaderMagicOffset:HeaderMagicOffset+xpage.MagicSize], xpage.Magic) {
		return xerr.InvalidDatabase.New("not a valid datafile, or the password is wrong")
	}
	if page[HeaderVersionOffset] != FormatVersion {
		return xerr.InvalidDatabase.Newf("format version is %d, not %d: not a valid datafile, or the password is wrong", page[HeaderVersionOffset], FormatVersion)
	}
	return nil
}

// AlignPhysicalSize 把物理文件长度向下取整到整页。
//
// 尾部不足一页的残留（写了一半就断电）直接当不存在。
func AlignPhysicalSize(size int64) int64 {
	return size - size%PageSize
}
