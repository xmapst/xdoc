package xpage

// dataFreeSlots 是数据页按剩余空间分档的门槛。
//
// 页按剩余空间挂进五条链（四个门槛分出五档），要放一篇文档时
// 直接找档次够的那条链，不必逐页去看还剩多少。
//
// 门槛是格式的一部分：链头存在集合页里，换一套门槛会让已有的
// 链挂在错误的档上。
var dataFreeSlots = [4]int{7344, 6120, 4896, 2448}

// DataFreeSlot 返回这一页按剩余空间该挂进哪一档。
func (p *Page) DataFreeSlot() uint8 { return dataFreeSlot(p.FreeBytes()) }

// dataFreeSlot 按剩余字节数算出档次，越小表示空间越多。
func dataFreeSlot(free int) uint8 {
	for i, threshold := range dataFreeSlots {
		if free >= threshold {
			return uint8(i)
		}
	}
	return uint8(len(dataFreeSlots))
}

// DataFreeSlotCount 是数据页空闲链的条数。
const DataFreeSlotCount = len(dataFreeSlots) + 1

// MinimumDataSlot 返回要放下 length 字节，至少该从哪一档找起。
//
// 减一是因为要的是「剩余空间严格多于 length」那一档：
// 恰好等于门槛的页放进去就一点不剩了。
func MinimumDataSlot(length int) int { return int(dataFreeSlot(length)) - 1 }

// MaxIndexNodeSize 是一个索引节点最多占多少字节。
const MaxIndexNodeSize = 1400

// IndexFreeSlot 返回索引页该挂进哪一档。
//
// 索引页只分两档：放得下一个最大节点，或者放不下。
// 节点大小上限固定，所以再细分没有意义。
func (p *Page) IndexFreeSlot() uint8 {
	free := p.FreeBytes()
	if free >= MaxIndexNodeSize {
		return 0
	}
	return 1
}
