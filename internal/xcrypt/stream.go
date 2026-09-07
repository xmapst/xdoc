package xcrypt

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"sync"

	"github.com/xmapst/xdoc/internal/xerr"
)

// File 是 [Stream] 对底层文件的要求：能按偏移随机读写、能截断、能问长度。
//
// 没有 Close：谁打开的谁负责关。
type File interface {
	io.ReaderAt
	io.WriterAt

	Truncate(size int64) error

	Stat() (fs.FileInfo, error)
}

// ErrCheckBlockCleared 表示盐页里的校验块全是零。
//
// 正常写过的校验块不可能全零。出现这种情况说明文件已经损坏；此时若放行，
// 接下来的写入会用当前口令覆盖掉原本的校验块，原口令就再也验不过了——
// 所以宁可拒绝打开。
var ErrCheckBlockCleared = xerr.InvalidDatabase.New("password check block is all zeros: the file is damaged; refusing to take it over with the current password, which would overwrite the original check block")

// Stream 把一份加密文件包装成从逻辑 0 开始的普通文件。
//
// 带位置的 Read/Write/Seek 不是并发安全的（位置和缓冲区都是共享状态）；
// 无位置的 ReadAt/WriteAt 可以并发，它们各自从池里取临时缓冲。
type Stream struct {
	f    File
	c    *Cipher
	salt [SaltSize]byte

	pos int64

	// buf 是 Read/Write 复用的中转缓冲，只随需要变大，不会缩回去。
	buf []byte
}

var _ io.ReadWriteSeeker = (*Stream)(nil)

// Open 打开一份加密文件；文件短于一页就当成新库，写出盐页。
//
// 已有文件要过三关：加密标志得是 [FlagEncrypted]，校验块不能全零，
// 校验块得与口令对得上。
func Open(f File, password string) (*Stream, error) {
	size, err := physicalSize(f)
	if err != nil {
		return nil, err
	}
	if size < PageSize {
		return create(f, password)
	}

	var head [SaltOffset + SaltSize]byte
	if _, err := f.ReadAt(head[:], 0); err != nil {
		return nil, xerr.InvalidDatabase.Wrap(err, "failed to read encryption flag and salt")
	}
	if head[FlagOffset] != FlagEncrypted {
		return nil, xerr.NotEncrypted.New("the file is not encrypted, but a password was given")
	}

	s := &Stream{f: f}
	copy(s.salt[:], head[SaltOffset:])

	if s.c, err = NewCipher(password, s.salt[:]); err != nil {
		return nil, err
	}

	var stored [CheckSize]byte
	if _, err := f.ReadAt(stored[:], CheckOffset); err != nil {
		return nil, xerr.InvalidDatabase.Wrap(err, "failed to read password check block")
	}
	if isAllZero(stored[:]) {
		return nil, ErrCheckBlockCleared
	}
	if err := s.c.VerifyCheckBlock(stored[:]); err != nil {
		return nil, err
	}
	return s, nil
}

// NewStream 用一把现成的密码包装另一份文件，跳过盐页的读写与口令校验。
//
// 给排序临时文件之类的旁路文件用：它们与主库同一把密钥，但各自的盐页
// 并不存在，逻辑数据仍从 [DataOffset] 起算。
func (c *Cipher) NewStream(f File, salt [SaltSize]byte) *Stream {
	return &Stream{f: f, c: c, salt: salt}
}

// create 生成盐、写出盐页，返回一个指向空数据区的流。
func create(f File, password string) (*Stream, error) {
	salt, err := NewSalt()
	if err != nil {
		return nil, err
	}
	c, err := NewCipher(password, salt[:])
	if err != nil {
		return nil, err
	}

	page := make([]byte, PageSize)
	page[FlagOffset] = FlagEncrypted
	copy(page[SaltOffset:], salt[:])
	check := c.CheckBlock()
	copy(page[CheckOffset:], check[:])

	if _, err := f.WriteAt(page, 0); err != nil {
		return nil, xerr.Unspecified.Wrap(err, "failed to write salt page")
	}
	return &Stream{f: f, c: c, salt: salt}, nil
}

// Salt 返回本流用的盐。
func (s *Stream) Salt() [SaltSize]byte { return s.salt }

// Cipher 返回本流用的密码，供旁路文件复用同一把密钥。
func (s *Stream) Cipher() *Cipher { return s.c }

// Length 返回逻辑长度：物理长度取整到页，再减去盐页。不足一页时算 0。
func (s *Stream) Length() (int64, error) {
	size, err := physicalSize(s.f)
	if err != nil {
		return 0, err
	}
	n := AlignPhysicalSize(size) - DataOffset
	if n < 0 {
		return 0, nil
	}
	return n, nil
}

// SetLength 按逻辑长度截断文件，物理上会多出一页盐页。
func (s *Stream) SetLength(v int64) error {
	if v < 0 {
		return xerr.InvalidDatafileState.Newf("logical length must not be negative: %d", v)
	}
	if err := s.f.Truncate(v + DataOffset); err != nil {
		return xerr.Unspecified.Wrapf(err, "failed to set file length to %d", v+DataOffset)
	}
	return nil
}

// Position 返回当前逻辑读写位置。
func (s *Stream) Position() int64 { return s.pos }

// Read 从当前位置读一段，位置与长度都必须按 [BlockSize] 对齐。
//
// 底层读到的字节数向下取整到整块——尾部不足一块的残留读不出来。
// 一块都凑不齐时返回 [io.EOF]。首块等于空洞基准时整段清零，见 [Cipher] 的 blank 字段。
func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := s.checkAligned(len(p)); err != nil {
		return 0, err
	}

	buf := s.grow(len(p))
	n, err := s.f.ReadAt(buf, DataOffset+s.pos)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, xerr.InvalidDatafileState.Wrapf(err, "failed to read %d bytes at logical offset %d", len(p), s.pos)
	}

	n -= n % BlockSize
	if n == 0 {
		return 0, io.EOF
	}
	s.c.decrypt(p[:n], buf[:n])

	if n >= BlockSize && bytes.Equal(p[:BlockSize], s.c.blank[:]) {
		clear(p[:n])
	}

	s.pos += int64(n)
	return n, nil
}

// Write 从当前位置写一段，位置与长度都必须按 [BlockSize] 对齐。
func (s *Stream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := s.checkAligned(len(p)); err != nil {
		return 0, err
	}
	buf := s.grow(len(p))
	s.c.encrypt(buf, p)
	if _, err := s.f.WriteAt(buf, DataOffset+s.pos); err != nil {
		return 0, xerr.Unspecified.Wrapf(err, "failed to write %d bytes at logical offset %d", len(p), s.pos)
	}
	s.pos += int64(len(p))
	return len(p), nil
}

// Seek 移动逻辑位置。
//
// 允许移到文件末尾之外，写入时会把中间撑开。
func (s *Stream) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = s.pos + offset
	case io.SeekEnd:
		length, err := s.Length()
		if err != nil {
			return 0, err
		}
		abs = length + offset
	default:
		return 0, xerr.InvalidDatafileState.Newf("unknown seek whence: %d", whence)
	}
	if abs < 0 {
		return 0, xerr.InvalidDatafileState.Newf("seek to negative offset: %d", abs)
	}
	s.pos = abs
	return abs, nil
}

// checkAligned 校验当前位置与本次长度都按块对齐。
func (s *Stream) checkAligned(n int) error {
	if s.pos%BlockSize != 0 {
		return xerr.InvalidDatafileState.Newf("logical position %d is not on a %d byte block boundary", s.pos, BlockSize)
	}
	if n%BlockSize != 0 {
		return xerr.InvalidDatafileState.Newf("length %d is not a multiple of block size %d", n, BlockSize)
	}
	return nil
}

// grow 把中转缓冲扩到至少 n 字节并切出前 n 字节。
func (s *Stream) grow(n int) []byte {
	if cap(s.buf) < n {
		s.buf = make([]byte, n)
	}
	return s.buf[:n]
}

// physicalSize 问底层文件要真实字节数，含盐页。
func physicalSize(f File) (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, xerr.Unspecified.Wrap(err, "failed to read file length")
	}
	return st.Size(), nil
}

// isAllZero 判断一段字节是否全为零。
func isAllZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// bufPool 是按需扩容的字节缓冲池，给能并发的 ReadAt/WriteAt 用。
type bufPool struct {
	p sync.Pool
}

// transit 是全进程共用的中转缓冲池。
var transit = bufPool{p: sync.Pool{New: func() any { b := make([]byte, 0); return &b }}}

// get 取一块至少 n 字节的缓冲，切好长度返回。
func (b *bufPool) get(n int) *[]byte {
	p := b.p.Get().(*[]byte)
	if cap(*p) < n {
		*p = make([]byte, n)
	}
	*p = (*p)[:n]
	return p
}

// put 把缓冲还回池里。
func (b *bufPool) put(p *[]byte) { b.p.Put(p) }

// ReadAt 从指定逻辑偏移读一段，不动当前位置，可并发调用。
//
// 与 [Stream.Read] 不同：读不满 p 时除了返回已读字节数，还会带上 [io.EOF]。
func (s *Stream) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := checkAlignedAt(off, len(p)); err != nil {
		return 0, err
	}
	buf := transit.get(len(p))
	defer transit.put(buf)

	n, err := s.f.ReadAt(*buf, DataOffset+off)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, xerr.InvalidDatafileState.Wrapf(err, "failed to read %d bytes at logical offset %d", len(p), off)
	}

	n -= n % BlockSize
	if n == 0 {
		return 0, io.EOF
	}
	s.c.decrypt(p[:n], (*buf)[:n])

	if n >= BlockSize && bytes.Equal(p[:BlockSize], s.c.blank[:]) {
		clear(p[:n])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// WriteAt 往指定逻辑偏移写一段，不动当前位置，可并发调用。
func (s *Stream) WriteAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := checkAlignedAt(off, len(p)); err != nil {
		return 0, err
	}
	buf := transit.get(len(p))
	defer transit.put(buf)

	s.c.encrypt(*buf, p)
	if _, err := s.f.WriteAt(*buf, DataOffset+off); err != nil {
		return 0, xerr.Unspecified.Wrapf(err, "failed to write %d bytes at logical offset %d", len(p), off)
	}
	return len(p), nil
}

// checkAlignedAt 校验给定偏移非负、按块对齐，且长度是块的整数倍。
func checkAlignedAt(off int64, n int) error {
	if off < 0 {
		return xerr.InvalidDatafileState.Newf("negative logical offset: %d", off)
	}
	if off%BlockSize != 0 {
		return xerr.InvalidDatafileState.Newf("logical offset %d is not on a %d byte block boundary", off, BlockSize)
	}
	if n%BlockSize != 0 {
		return xerr.InvalidDatafileState.Newf("length %d is not a multiple of block size %d", n, BlockSize)
	}
	return nil
}
