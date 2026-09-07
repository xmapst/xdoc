package xpage

import (
	"fmt"
	"slices"
)

// validateSpan 是一段占用的字节区间，检查重叠时用。
type validateSpan struct{ lo, hi int }

// Validate 检查一页的内部账目是否自洽。
//
// 页头里的几个计数（项数、已用、碎片、下一个空闲位置、最大槽号）
// 彼此之间有确定的关系，而且要与槽表实际记录的内容对得上。
// 任何一处对不上都说明这一页坏了。
//
// 逐项检查：空页的各计数必须都是零；下一个空闲位置必须等于
// 页头加已用加碎片；每个在用的槽都要落在合法范围内；
// 在用槽数与项数要相等；各段长度之和要等于已用字节数。
//
// 最后把各段按起点排序，检查两两不重叠——重叠意味着两项占了同一块字节，
// 改一项会连带改坏另一项。
func (p *Page) Validate() error {
	items, used, frag := p.ItemsCount(), p.UsedBytes(), p.FragmentedBytes()
	next, hi := p.NextFreePosition(), p.HighestIndex()

	if hi != EmptyIndex && hi > MaxSlotIndex {
		return fmt.Errorf("%w: highest index %d exceeds %d", ErrCorrupt, hi, MaxSlotIndex)
	}
	if items > MaxItems {
		return fmt.Errorf("%w: items count %d exceeds %d", ErrCorrupt, items, MaxItems)
	}

	if (items == 0) != (hi == EmptyIndex) || (items == 0) != (used == 0) {
		return fmt.Errorf("%w: items=%d highest=%d used=%d disagree on emptiness",
			ErrCorrupt, items, hi, used)
	}
	if items == 0 {
		if next != HeaderSize || frag != 0 {
			return fmt.Errorf("%w: empty page has next=%d fragmented=%d, want %d and 0",
				ErrCorrupt, next, frag, HeaderSize)
		}
		return nil
	}

	if next != HeaderSize+used+frag {
		return fmt.Errorf("%w: next free position %d != %d + %d + %d",
			ErrCorrupt, next, HeaderSize, used, frag)
	}
	footer := p.FooterSize()

	if next < HeaderSize || next > PageSize-footer {
		return fmt.Errorf("%w: next free position %d out of range [%d,%d]",
			ErrCorrupt, next, HeaderSize, PageSize-footer)
	}

	if free := PageSize - HeaderSize - used - footer; free < frag {
		return fmt.Errorf("%w: free %d less than fragmented %d", ErrCorrupt, free, frag)
	}

	if pos, _ := p.slot(hi); pos == 0 {
		return fmt.Errorf("%w: highest index %d points at an unused slot", ErrCorrupt, hi)
	}

	type span = validateSpan
	spans := make([]span, 0, items)
	count, sum := 0, 0
	for i := 0; i <= int(hi); i++ {
		pos, length := p.slot(uint8(i))
		if pos == 0 {
			if length != 0 {
				return fmt.Errorf("%w: slot %d has length %d but no position", ErrCorrupt, i, length)
			}
			continue
		}
		if err := p.checkSegment(uint8(i), pos, length); err != nil {
			return err
		}
		if pos+length > next {
			return fmt.Errorf("%w: slot %d ends at %d, past next free position %d",
				ErrCorrupt, i, pos+length, next)
		}
		spans = append(spans, span{pos, pos + length})
		count++
		sum += length
	}
	if count != items {
		return fmt.Errorf("%w: %d slots in use but items count says %d", ErrCorrupt, count, items)
	}
	if sum != used {
		return fmt.Errorf("%w: segment lengths total %d but used bytes says %d", ErrCorrupt, sum, used)
	}

	slices.SortFunc(spans, func(a, b span) int { return a.lo - b.lo })
	for i := 1; i < len(spans); i++ {
		if spans[i].lo < spans[i-1].hi {
			return fmt.Errorf("%w: segments [%d,%d) and [%d,%d) overlap",
				ErrCorrupt, spans[i-1].lo, spans[i-1].hi, spans[i].lo, spans[i].hi)
		}
	}
	return nil
}
