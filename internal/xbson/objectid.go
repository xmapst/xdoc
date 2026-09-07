package xbson

import (
	"cmp"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"os"
	"sync/atomic"
	"time"
)

// ObjectID 是 12 字节的主键：4 字节时间戳 + 3 字节机器标识 +
// 2 字节进程号 + 3 字节计数器。
//
// 带时间戳意味着它大致按生成顺序递增，插入时因此落在主键索引的尾部，
// 不必在中间腾位置。
type ObjectID [12]byte

// ObjectIDNil 是全零的 [ObjectID]。
var ObjectIDNil ObjectID

var (
	// 机器标识、进程号与计数器，进程启动时定下。
	oidMachine [3]byte
	oidPID     [2]byte
	oidCounter atomic.Uint32
)

// init 定下机器标识、进程号与计数器初值。
//
// 机器标识取主机名的散列，取不到主机名就用随机字节——两台机器算出同一个
// 标识只会让并发生成时多依赖一点计数器，不会撞主键。
//
// 计数器从随机值起步，而不是从零：同一台机器上前后两次运行的进程号
// 可能相同，从零起步会让两次运行生成出同样的一串标识。
func init() {
	h := fnv.New32a()
	if name, err := os.Hostname(); err == nil {
		_, _ = h.Write([]byte(name))
	} else {
		var b [8]byte
		_, _ = rand.Read(b[:])
		_, _ = h.Write(b[:])
	}
	sum := h.Sum32() & 0x00FFFFFF
	oidMachine = [3]byte{byte(sum >> 16), byte(sum >> 8), byte(sum)}

	pid := os.Getpid()
	oidPID = [2]byte{byte(pid >> 8), byte(pid)}

	var seed [4]byte
	_, _ = rand.Read(seed[:])
	oidCounter.Store(binary.BigEndian.Uint32(seed[:]) & 0x00FFFFFF)
}

// NewObjectID 生成一个新的 [ObjectID]。
func NewObjectID() ObjectID {
	return newObjectIDAt(time.Now())
}

// newObjectIDAt 用指定时刻生成一个 [ObjectID]。
//
// 计数器是原子自增并截到 24 位，所以同一秒内可以生成一千六百多万个
// 互不相同的标识。
func newObjectIDAt(now time.Time) ObjectID {
	var id ObjectID
	binary.BigEndian.PutUint32(id[0:4], uint32(now.UTC().Unix()))
	copy(id[4:7], oidMachine[:])
	copy(id[7:9], oidPID[:])
	c := oidCounter.Add(1) & 0x00FFFFFF
	id[9], id[10], id[11] = byte(c>>16), byte(c>>8), byte(c)
	return id
}

// ObjectIDFromBytes 从 12 字节读出一个 [ObjectID]。
func ObjectIDFromBytes(b []byte) (ObjectID, error) {
	if len(b) < 12 {
		return ObjectID{}, fmt.Errorf("xbson: object id needs 12 bytes, got %d", len(b))
	}
	var id ObjectID
	copy(id[:], b[:12])
	return id, nil
}

// ParseObjectID 解析 24 个十六进制字符。
func ParseObjectID(s string) (ObjectID, error) {
	if len(s) != 24 {
		return ObjectID{}, fmt.Errorf("xbson: invalid object id %q: want 24 hex chars, got %d", s, len(s))
	}
	var id ObjectID
	if _, err := hex.Decode(id[:], []byte(s)); err != nil {
		return ObjectID{}, fmt.Errorf("xbson: invalid object id %q: %w", s, err)
	}
	return id, nil
}

// String 写成 24 个小写十六进制字符。
func (id ObjectID) String() string { return hex.EncodeToString(id[:]) }

// Timestamp 取出生成时刻，精确到秒。
func (id ObjectID) Timestamp() time.Time {
	return time.Unix(int64(binary.BigEndian.Uint32(id[0:4])), 0).UTC()
}

// Compare 分四段比较，而不是整体按字节比。
//
// **时间戳与进程号按有符号数比**，机器标识与计数器按无符号数比。
// 这是格式定下的次序，索引就是按它排的。
//
// 有符号的时间戳有个后果：2038 年之后生成的标识，其时间戳的最高位为 1，
// 按有符号数比会排在更早的标识**前面**。
func (id ObjectID) Compare(o ObjectID) int {
	if c := cmp.Compare(int32(binary.BigEndian.Uint32(id[0:4])), int32(binary.BigEndian.Uint32(o[0:4]))); c != 0 {
		return c
	}

	if c := cmp.Compare(u24(id[4:7]), u24(o[4:7])); c != 0 {
		return c
	}
	if c := cmp.Compare(int32(int16(binary.BigEndian.Uint16(id[7:9]))),
		int32(int16(binary.BigEndian.Uint16(o[7:9])))); c != 0 {
		return c
	}
	return cmp.Compare(u24(id[9:12]), u24(o[9:12]))
}

// u24 把 3 个字节读成一个无符号 24 位数。
func u24(b []byte) int32 { return int32(b[0])<<16 | int32(b[1])<<8 | int32(b[2]) }
