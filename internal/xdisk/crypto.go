package xdisk

import (
	"fmt"
	"os"

	"github.com/xmapst/xdoc/internal/xcrypt"
)

// encryptedStorage 把加密流适配成 [Storage]。
//
// 长度与截断都走加密流的逻辑长度（不含盐页），落盘和关闭直接作用于底层文件。
type encryptedStorage struct {
	f *os.File
	s *xcrypt.Stream
}

// OpenEncryptedFile 打开一份口令加密的文件。
//
// 文件不存在或短于一页时会写出盐页，当成新库；已有文件则要过口令校验。
// 与 [OpenFile] 一样，只读模式下不带 O_CREATE。
func OpenEncryptedFile(path, password string, readOnly bool) (Storage, error) {
	flag := os.O_RDWR | os.O_CREATE
	if readOnly {
		flag = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		return nil, fmt.Errorf("xdisk: open %q: %w", path, err)
	}
	s, err := xcrypt.Open(f, password)
	if err != nil {
		return nil, fmt.Errorf("xdisk: open encrypted %q: %w", path, err)
	}
	return &encryptedStorage{f: f, s: s}, nil
}

// ReadAt 按逻辑偏移读一段并解密。偏移与长度都必须按加密块对齐。
func (e *encryptedStorage) ReadAt(p []byte, off int64) (int, error) { return e.s.ReadAt(p, off) }

// WriteAt 加密一段并按逻辑偏移写下。约束同 ReadAt。
func (e *encryptedStorage) WriteAt(p []byte, off int64) (int, error) { return e.s.WriteAt(p, off) }

// Size 返回逻辑长度，不含盐页。
func (e *encryptedStorage) Size() (int64, error) { return e.s.Length() }

// Truncate 按逻辑长度截断，物理文件会比它多一页盐页。
func (e *encryptedStorage) Truncate(n int64) error { return e.s.SetLength(n) }

// Sync 把底层文件刷到设备。
func (e *encryptedStorage) Sync() error { return e.f.Sync() }

// Close 关闭底层文件。
func (e *encryptedStorage) Close() error { return e.f.Close() }
