package xdoc

import (
	"context"
	"fmt"
	"iter"
	"path/filepath"
	"strings"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xsysfile"
)

// fileFormat 从 $file 的参数里取出格式与文件名。
//
// filename 必填且必须是字符串；format 不给时从扩展名推，
// 所以 $file('out.csv') 走 CSV、$file('out.json') 走 JSON。
func (so sysOpts) fileFormat() (format, filename string, err error) {
	fn, err := so.option("filename", nil)
	if err != nil {
		return "", "", err
	}
	if fn == nil {
		return "", "", fmt.Errorf(
			"Collection $file requires string as 'filename' or a document field 'filename'")
	}
	filename, ok := fn.AsString()
	if !ok {
		return "", "", fmt.Errorf(
			"Collection $file requires string as 'filename' or a document field 'filename', got %s",
			fn.Type())
	}
	f, err := so.option("format", xbson.String(filepath.Ext(filename)))
	if err != nil {
		return "", "", err
	}
	format, _ = f.AsString()
	format = strings.TrimPrefix(format, ".")
	return format, filename, nil
}

// fileOptions 把 $file 的其余参数整理成读写选项，各有默认值。
//
// delimiter 只取第一个字节，空串报错——CSV 没有"没有分隔符"这种写法。
func (so sysOpts) fileOptions(filename string) (xsysfile.Options, error) {
	o := xsysfile.Options{Filename: filename}

	enc, err := so.option("encoding", xbson.String("utf-8"))
	if err != nil {
		return o, err
	}
	o.Encoding, _ = enc.AsString()

	pretty, err := so.option("pretty", xbson.False)
	if err != nil {
		return o, err
	}
	o.Pretty, _ = pretty.AsBoolean()

	indent, err := so.option("indent", xbson.Int32(4))
	if err != nil {
		return o, err
	}
	n, _ := indent.AsInt32()
	o.Indent = int(n)

	over, err := so.option("overwritten", xbson.False)
	if err != nil {
		return o, err
	}
	o.Overwritten, _ = over.AsBoolean()

	delim, err := so.option("delimiter", xbson.String(","))
	if err != nil {
		return o, err
	}
	ds, _ := delim.AsString()
	if ds == "" {
		return o, fmt.Errorf("Parameter `delimiter` must not be empty")
	}
	o.Delimiter = ds[0]
	return o, nil
}

// sysFileInput 把一份 JSON 或 CSV 文件当集合读。
//
// CSV 的 header 在**读**侧是列名数组，在写侧却是「要不要写表头」的布尔量
// ——同名不同义，这处不对称是格式定死的，所以两边分开取。
//
// CSV 读回来的值全是字符串，没有类型推断：1 读回来是 "1"。
func (*DB) sysFileInput(_ context.Context, _ *Tx, opts sysOpts) iter.Seq2[*Document, error] {
	format, filename, err := opts.fileFormat()
	if err != nil {
		return seqErr[*Document](err)
	}
	o, err := opts.fileOptions(filename)
	if err != nil {
		return seqErr[*Document](err)
	}
	switch strings.ToLower(format) {
	case "json":
		return o.ReadJSON()
	case "csv":
		if opts.v != nil && opts.v.Type() == xbson.TypeDocument {
			d, _ := opts.v.AsDocument()
			if arr, ok := d.Get("header").AsArray(); ok {
				for _, v := range arr.Items() {
					s, _ := v.AsString()
					o.CSVHeader = append(o.CSVHeader, s)
				}
			}
		}
		return o.ReadCSV()
	}

	return seqErr[*Document](fmt.Errorf("Unknow file format in $file: `%s`", format))
}

// sysFileOutput 把查询结果写成一份 JSON 或 CSV 文件，返回写了几篇。
//
// 写文件不在事务里：文件系统没有参与两阶段提交的办法，所以 BEGIN 之后导出、
// 再 ROLLBACK，文件仍然在那儿。
func (*DB) sysFileOutput(_ context.Context, _ *Tx, opts sysOpts, docs iter.Seq2[*Document, error]) (int, error) {
	format, filename, err := opts.fileFormat()
	if err != nil {
		return 0, err
	}
	o, err := opts.fileOptions(filename)
	if err != nil {
		return 0, err
	}
	switch strings.ToLower(format) {
	case "json":
		return o.WriteJSON(docs)
	case "csv":
		h, err := opts.option("header", xbson.True)
		if err != nil {
			return 0, err
		}
		o.WriteHeader, _ = h.AsBoolean()
		return o.WriteCSV(docs)
	}
	return 0, fmt.Errorf("Unknow file format in $file: `%s`", format)
}
