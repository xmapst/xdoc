package xdisk

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Storage 是一块可随机读写、可截断、可落盘的字节区域。
//
// [Disk] 只认这个接口，所以数据文件既可以是真文件（[FileStorage]）、
// 纯内存（[MemStorage]），也可以是加密文件（见 [OpenEncryptedFile]）。
type Storage interface {
	io.ReaderAt
	io.WriterAt

	Size() (int64, error)

	Truncate(n int64) error

	Sync() error

	Close() error
}

// FileStorage 把一个 [os.File] 适配成 [Storage]。
type FileStorage struct {
	f        *os.File
	readOnly bool
}

// OpenFile 打开或创建一个文件。
//
// 只读模式下不带 O_CREATE：文件不存在就直接报错，不会凭空造出一个空库。
func OpenFile(path string, readOnly bool) (*FileStorage, error) {
	flag := os.O_RDWR | os.O_CREATE
	if readOnly {
		flag = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		return nil, fmt.Errorf("xdisk: open %q: %w", path, err)
	}
	return &FileStorage{f: f, readOnly: readOnly}, nil
}

// ReadAt 从指定偏移读，语义同 [os.File.ReadAt]。
func (s *FileStorage) ReadAt(p []byte, off int64) (int, error) { return s.f.ReadAt(p, off) }

// WriteAt 往指定偏移写，语义同 [os.File.WriteAt]。
func (s *FileStorage) WriteAt(p []byte, off int64) (int, error) { return s.f.WriteAt(p, off) }

// Size 返回文件当前字节数。
func (s *FileStorage) Size() (int64, error) {
	fi, err := s.f.Stat()
	if err != nil {
		return 0, fmt.Errorf("xdisk: stat %q: %w", s.f.Name(), err)
	}
	return fi.Size(), nil
}

// Truncate 改变文件长度；只读时报错。
func (s *FileStorage) Truncate(n int64) error {
	if s.readOnly {
		return fmt.Errorf("xdisk: %q is open read-only", s.f.Name())
	}
	return s.f.Truncate(n)
}

// Sync 把内核缓冲刷到设备；只读时什么也不做。
func (s *FileStorage) Sync() error {
	if s.readOnly {
		return nil
	}
	return s.f.Sync()
}

// Close 关闭底层文件。
func (s *FileStorage) Close() error { return s.f.Close() }

// Name 返回文件路径，[Disk.Names] 会用到。
func (s *FileStorage) Name() string { return s.f.Name() }

// MemStorage 是内存里的 [Storage]，给内存库和测试用。
//
// 自带读写锁，可以并发读写；数据随进程一起消失。
type MemStorage struct {
	mu  sync.RWMutex
	buf []byte
}

// NewMemStorage 造一块空的内存存储。
func NewMemStorage() *MemStorage { return &MemStorage{} }

// ReadAt 从内存里拷一段；越过末尾返回 [io.EOF]，读不满也带上 [io.EOF]。
func (s *MemStorage) ReadAt(p []byte, off int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if off < 0 {
		return 0, fmt.Errorf("xdisk: negative offset %d", off)
	}
	if off >= int64(len(s.buf)) {
		return 0, io.EOF
	}
	n := copy(p, s.buf[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// WriteAt 往内存里写一段，越过末尾时自动补零扩容。
func (s *MemStorage) WriteAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if off < 0 {
		return 0, fmt.Errorf("xdisk: negative offset %d", off)
	}
	if end := off + int64(len(p)); end > int64(len(s.buf)) {
		s.buf = append(s.buf, make([]byte, end-int64(len(s.buf)))...)
	}
	return copy(s.buf[off:], p), nil
}

// Size 返回当前字节数。
func (s *MemStorage) Size() (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return int64(len(s.buf)), nil
}

// Truncate 改变长度：变短时把砍掉的部分清零再切片，变长时补零。
func (s *MemStorage) Truncate(n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case n < 0:
		return fmt.Errorf("xdisk: negative length %d", n)
	case n <= int64(len(s.buf)):
		clear(s.buf[n:])
		s.buf = s.buf[:n]
	default:
		s.buf = append(s.buf, make([]byte, n-int64(len(s.buf)))...)
	}
	return nil
}

// Sync 无事可做。
func (s *MemStorage) Sync() error { return nil }

// Close 丢掉内容。关掉之后所有读都返回 [io.EOF]。
func (s *MemStorage) Close() error { s.mu.Lock(); s.buf = nil; s.mu.Unlock(); return nil }

// ErrShortRead 表示按整页读却没读满：文件被截断了，或者页号越过了末页。
var ErrShortRead = errors.New("xdisk: short read")

// file 给 [Storage] 添两个内部便利方法。
type file struct{ Storage }

// name 返回底层路径；内存存储之类没有名字的返回空串。
func (f file) name() string {
	if n, ok := f.Storage.(interface{ Name() string }); ok {
		return n.Name()
	}
	return ""
}

// readFull 要么读满整个 p，要么报错。
//
// 读不满时一律折成 [ErrShortRead]，包括底层给的 [io.EOF]——按页读时
// 「只读到半页」和「什么也没读到」是同一类问题。
func (f file) readFull(p []byte, off int64) error {
	n, err := f.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: got %d of %d bytes at offset %d", ErrShortRead, n, len(p), off)
	}
	return err
}

// LogPath 由数据文件路径推出日志文件路径。
func LogPath(dataPath string) string { return suffixPath(dataPath, "-log") }

// TempPath 由数据文件路径推出临时文件路径，重建库时用。
func TempPath(dataPath string) string { return suffixPath(dataPath, "-tmp") }

// BackupPath 由数据文件路径推出备份文件路径。
func BackupPath(dataPath string) string { return suffixPath(dataPath, "-backup") }

// suffixPath 把后缀插在扩展名之前：a/b.db 加 -log 得到 a/b-log.db。
//
// 插在扩展名前而不是末尾，是为了让这些附属文件保持同样的扩展名。
func suffixPath(p, suffix string) string {
	dir, base := filepath.Split(p)
	ext := filepath.Ext(base)
	return filepath.Join(dir, strings.TrimSuffix(base, ext)+suffix+ext)
}

// EmptyStorage 是一块永远为空、不许写的 [Storage]。
//
// 只读打开一份没有日志文件的库时，拿它顶替日志：读什么都是 [io.EOF]，
// 写就报错。
type EmptyStorage struct{}

// ReadAt 一律返回 [io.EOF]；读零字节除外。
func (EmptyStorage) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("xdisk: negative offset %d", off)
	}
	if len(p) == 0 {
		return 0, nil
	}
	return 0, io.EOF
}

// WriteAt 一律报错。
func (EmptyStorage) WriteAt([]byte, int64) (int, error) {
	return 0, errors.New("xdisk: storage is empty and read-only")
}

// Size 恒为 0。
func (EmptyStorage) Size() (int64, error) { return 0, nil }

// Truncate 只接受 0，其余长度报错。
func (EmptyStorage) Truncate(n int64) error {
	if n == 0 {
		return nil
	}
	return errors.New("xdisk: storage is empty and read-only")
}

// Sync 无事可做。
func (EmptyStorage) Sync() error { return nil }

// Close 无事可做。
func (EmptyStorage) Close() error { return nil }
