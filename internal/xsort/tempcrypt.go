package xsort

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/xmapst/xdoc/internal/xdisk"
)

// tempCipher 给排序临时文件加一层流式加密。
//
// 它包在存储之上，读写时按位置生成密钥流异或——所以随机读写仍然成立，
// 不必按块对齐。
type tempCipher struct {
	st    xdisk.Storage
	block cipher.Block
	nonce uint64
}

// newTempCipher 用口令派生密钥，并随机取一个前缀。
//
// 临时文件的生命周期只有一次排序，进程一结束就删了，所以密钥流的
// 前缀每次运行都重新随机，不必也不该持久化。
func newTempCipher(st xdisk.Storage, password string) (*tempCipher, error) {
	key := sha256.Sum256([]byte(password))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("xsort: temp cipher: %w", err)
	}
	var n [8]byte
	if _, err := rand.Read(n[:]); err != nil {
		return nil, fmt.Errorf("xsort: temp cipher nonce: %w", err)
	}
	return &tempCipher{st: st, block: block, nonce: binary.BigEndian.Uint64(n[:])}, nil
}

// xor 按位置生成密钥流并异或。
//
// 计数器由「随机前缀 + 块序号」拼成，块序号从文件偏移算出来，
// 所以同一个位置每次算出同样的密钥流，随机读写因此成立。
func (c *tempCipher) xor(p []byte, off int64) {
	const bs = aes.BlockSize
	var ctr, ks [bs]byte
	binary.BigEndian.PutUint64(ctr[0:8], c.nonce)
	for i := 0; i < len(p); {
		pos := off + int64(i)
		binary.BigEndian.PutUint64(ctr[8:16], uint64(pos/bs))
		c.block.Encrypt(ks[:], ctr[:])

		start := int(pos % bs)
		n := min(len(p)-i, bs-start)
		for j := range n {
			p[i+j] ^= ks[start+j]
		}
		i += n
	}
}

// ReadAt 读出并解密。
func (c *tempCipher) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.st.ReadAt(p, off)
	if n > 0 {
		c.xor(p[:n], off)
	}
	return n, err
}

// WriteAt 加密之后写出。
//
// 先拷一份再异或：调用方给的切片不该被就地改成密文。
func (c *tempCipher) WriteAt(p []byte, off int64) (int, error) {
	buf := make([]byte, len(p))
	copy(buf, p)
	c.xor(buf, off)
	return c.st.WriteAt(buf, off)
}

// 其余操作直接转发给底层存储。
func (c *tempCipher) Size() (int64, error)   { return c.st.Size() }
func (c *tempCipher) Truncate(n int64) error { return c.st.Truncate(n) }
func (c *tempCipher) Sync() error            { return c.st.Sync() }
func (c *tempCipher) Close() error           { return c.st.Close() }
