package xmap

import (
	"fmt"
	"strings"
)

// TagKey 是结构体标签的键名。
const TagKey = "bson"

// idFieldName 是主键在文档里的字段名。
const idFieldName = "_id"

// tagSpec 是一条 bson 标签解析出来的东西。
type tagSpec struct {
	name      string
	hasName   bool
	ignore    bool
	omitEmpty bool
	omitZero  bool
	isID      bool
	autoID    bool
	isRef     bool
	ref       string
	inline    bool
	vector    bool
}

// parseTag 解析一条 bson 标签。
//
// 写成 -  的字段整个跳过。名字之后用逗号分隔各个选项：
// omitempty、omitzero、id、noauto、ref[=集合名]、inline、vector。
// 名字叫 _id（不分大小写）等同于加了 id 选项。
//
// 自动主键默认开着，写 noauto 才关掉。inline 与 id、ref 互斥。
func parseTag(raw string, present bool) (tagSpec, error) {
	spec := tagSpec{autoID: true}
	if !present || raw == "" {
		return spec, nil
	}

	if raw == "-" {
		spec.ignore = true
		return spec, nil
	}
	name, rest, _ := strings.Cut(raw, ",")
	if name != "" {
		if err := validName(name); err != nil {
			return spec, err
		}
		spec.name = name
		spec.hasName = true
		if strings.EqualFold(name, idFieldName) {
			spec.isID = true
		}
	}
	for opt := range strings.SplitSeq(rest, ",") {
		if opt == "" {
			continue
		}
		key, val, hasVal := strings.Cut(opt, "=")
		switch key {
		case "omitempty":
			spec.omitEmpty = true
		case "omitzero":
			spec.omitZero = true
		case "id":
			spec.isID = true
		case "noauto":
			spec.autoID = false
		case "ref":
			spec.isRef = true
			if hasVal {
				spec.ref = val
			}
		case "inline":
			spec.inline = true
		case "vector":
			spec.vector = true
		default:
			return spec, fmt.Errorf("%w: unknown option %q", ErrInvalidTag, opt)
		}
	}
	if spec.inline && (spec.isRef || spec.isID) {
		return spec, fmt.Errorf("%w: inline cannot be combined with id/ref", ErrInvalidTag)
	}
	return spec, nil
}

// validName 校验文档字段名：不能含点，不能以美元号开头——那两样在表达式里另有含义。
func validName(name string) error {
	if strings.ContainsAny(name, ".") {
		return fmt.Errorf("%w: field name %q must not contain a dot", ErrInvalidTag, name)
	}
	if strings.HasPrefix(name, "$") {
		return fmt.Errorf("%w: field name %q must not start with $", ErrInvalidTag, name)
	}
	return nil
}

// foldName 把名字折成大写，用来做不分大小写的字段匹配。
//
// 只折 ASCII 小写字母；没有小写字母时原样返回，省掉一次分配。
func foldName(s string) string {
	need := false
	for i := range len(s) {
		if c := s[i]; c >= 'a' && c <= 'z' {
			need = true
			break
		}
	}
	if !need {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}
