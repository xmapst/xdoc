package xdoc

import "github.com/xmapst/xdoc/internal/xcoll"

// Collation 是字符串比较规则：一个区域号加一组选项位。
//
// 它决定索引里字符串键的物理顺序，所以是数据文件的一部分——规则连同它的标识
// 写进文件头，打开既有库时按文件头里那条来，不按本进程的偏好。
type Collation = xcoll.Collation

// 比较选项的位标志，取值与写进文件头的那个整数一致，按位或起来交给 [NewCollation]。
//
// 其中四位没有对应实现（忽略符号、假名类型、数字顺序、标点优先），读到时按忽略
// 这一位处理——能打开它，好过让一个只是选项位填得不同的库根本打不开。
const (
	// CompareNone 是区分大小写、区分重音的语言学比较。
	CompareNone = xcoll.OptNone

	// CompareIgnoreCase 忽略大小写。
	CompareIgnoreCase = xcoll.OptIgnoreCase

	// CompareIgnoreNonSpace 忽略变音符号。
	CompareIgnoreNonSpace = xcoll.OptIgnoreNonSpace

	// CompareIgnoreSymbols 忽略符号。没有对应实现，读到时按忽略这一位处理。
	CompareIgnoreSymbols = xcoll.OptIgnoreSymbols

	// CompareIgnoreKanaType 不区分平假名与片假名。没有对应实现，读到时按忽略处理。
	CompareIgnoreKanaType = xcoll.OptIgnoreKanaType

	// CompareIgnoreWidth 不区分全角与半角。
	CompareIgnoreWidth = xcoll.OptIgnoreWidth

	// CompareNumericOrdering 把字符串里的数字段按数值排（a2 排在 a10 前面）。
	// 没有对应实现，读到时按忽略这一位处理。
	CompareNumericOrdering = xcoll.OptNumericOrdering

	// CompareStringSort 把标点排在字母之前。没有对应实现，读到时按忽略这一位处理。
	CompareStringSort = xcoll.OptStringSort

	// CompareOrdinal 强制按 UTF-16 码元比较，不做任何语言学处理。
	CompareOrdinal = xcoll.OptOrdinal

	// CompareOrdinalIgnoreCase 按码元比较，但先折成大写。
	CompareOrdinalIgnoreCase = xcoll.OptOrdinalIgnoreCase
)

// DefaultCollation 是新建库用的规则：不依赖区域、按 UTF-16 码元比较，
// 因此区分大小写。
//
// 取码元序而不是某个区域的语言学序，是为了让顺序不随机器而变：语言学排序的结果
// 取决于国际化库的版本与实现，同一份代码在两台机器上可能建出键序不同的库，
// 而一台上写的库在另一台打开时会沿错误的分支二分查找，静默漏记录。
func DefaultCollation() Collation { return xcoll.Default }

// BinaryCollation 按字节逐个比较，连码元折算都不做。
func BinaryCollation() Collation { return xcoll.Binary }

// NewCollation 按区域号与选项构造一条规则。
//
// 区域号必须在区域表里，否则报错；选项位不挑剔，没有对应实现的那几位按忽略处理，
// 构造照样成功。宽严不同是因为后果不同：选项位只影响排出来的顺序，
// 而区域号是这条规则的身份，要原样写进文件头。
//
// 打开既有库不走这里：文件头里那两个整数是建库的一方写下的，认不出也得能打开，
// 那条路自己退回不依赖区域的一档。
func NewCollation(lcid, options int) (Collation, error) {
	return xcoll.New(lcid, options)
}

// ParseCollation 解析「区域/选项」写法，例如 "/Ordinal"、"en-US/IgnoreCase"、"127/None"。
//
// 斜杠前面既收语言标签（"en-US"，空串表示不依赖区域）也收区域号（"1033"）；
// 两者不会撞车，语言标签里不会只有数字。斜杠可以整个省掉，"en-US" 等同于
// "en-US/None"，选项名也不分大小写。
//
// 这几处放宽都只进不出：[Collation.String] 印区域时只印语言标签、印选项名时只印
// 标准拼写，所以宽松写法进得来、出不去，写进文件头的始终是标准形。
func ParseCollation(s string) (Collation, error) {
	return xcoll.Parse(s)
}
