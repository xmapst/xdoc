package xcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"fmt"

	"github.com/xmapst/xdoc/internal/xerr"
)

// NewSalt 取一份随机盐，建库时写进盐页。
func NewSalt() ([SaltSize]byte, error) {
	var salt [SaltSize]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return salt, xerr.Unspecified.Wrap(err, "failed to generate random salt")
	}
	return salt, nil
}

// Salt 是盐的切片形式，长度必须正好 [SaltSize]。
type Salt []byte

// DeriveKey 用 PBKDF2-HMAC-SHA1 把口令和盐派生成 AES 密钥。
//
// 摘要、迭代次数、密钥长度都是文件格式的一部分：任一项变了，同一个口令
// 就打不开原来的文件。
func (s Salt) DeriveKey(password string) ([KeySize]byte, error) {
	var key [KeySize]byte
	if len(s) != SaltSize {
		return key, xerr.InvalidDatabase.Newf("salt must be %d bytes, got %d", SaltSize, len(s))
	}
	dk, err := pbkdf2.Key(sha1.New, password, s, Iterations, KeySize)
	if err != nil {
		return key, xerr.Unspecified.Wrap(err, "failed to derive key")
	}
	copy(key[:], dk)
	return key, nil
}

// Cipher 是一把配好密钥的 AES 分组密码，带两份预算好的常量块。
type Cipher struct {
	block cipher.Block

	// check 是口令校验块的密文，建库时写盘、开库时比对。
	check [CheckSize]byte

	// blank 是全零密文块解出来的明文。
	//
	// 写盘时没被写过的区域在物理上是零；解密这样一块会得到一堆随机字节，
	// 而不是零。读取时拿首块与它比对，命中就把整段清零，让上层看到本该
	// 看到的零。代价是：明文恰好等于这个值的块也会被误当成空洞。
	blank [BlockSize]byte
}

// NewCipher 由口令和盐造一把密码，顺带把校验块与空洞基准算好。
func NewCipher(password string, salt Salt) (*Cipher, error) {
	key, err := salt.DeriveKey(password)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, xerr.Unspecified.Wrap(err, "failed to create block cipher")
	}
	c := &Cipher{block: block}

	var plain [CheckSize]byte
	for i := range plain {
		plain[i] = CheckPlainByte
	}
	c.encrypt(c.check[:], plain[:])

	var zero [BlockSize]byte
	c.block.Decrypt(c.blank[:], zero[:])

	return c, nil
}

// encrypt 逐块加密，不检查长度——调用方负责保证对齐。
func (c *Cipher) encrypt(dst, src []byte) {
	for i := 0; i < len(src); i += BlockSize {
		c.block.Encrypt(dst[i:i+BlockSize], src[i:i+BlockSize])
	}
}

// decrypt 逐块解密，不检查长度——调用方负责保证对齐。
func (c *Cipher) decrypt(dst, src []byte) {
	for i := 0; i < len(src); i += BlockSize {
		c.block.Decrypt(dst[i:i+BlockSize], src[i:i+BlockSize])
	}
}

// Encrypt 加密一段字节，长度须相等且是 [BlockSize] 的整数倍。dst 与 src 可以是同一块内存。
func (c *Cipher) Encrypt(dst, src []byte) error {
	if err := checkBlockArgs(dst, src); err != nil {
		return err
	}
	c.encrypt(dst, src)
	return nil
}

// Decrypt 解密一段字节，约束同 [Cipher.Encrypt]。
func (c *Cipher) Decrypt(dst, src []byte) error {
	if err := checkBlockArgs(dst, src); err != nil {
		return err
	}
	c.decrypt(dst, src)
	return nil
}

// checkBlockArgs 校验两段长度相等且都按块对齐。
func checkBlockArgs(dst, src []byte) error {
	if len(dst) != len(src) {
		return xerr.InvalidDatafileState.Newf("source and destination lengths differ: %d / %d", len(src), len(dst))
	}
	if len(src)%BlockSize != 0 {
		return xerr.InvalidDatafileState.Newf("length %d is not a multiple of block size %d", len(src), BlockSize)
	}
	return nil
}

// CheckBlock 返回该写进盐页的口令校验块。
func (c *Cipher) CheckBlock() [CheckSize]byte { return c.check }

// VerifyCheckBlock 拿盘上的校验块与本口令算出的比对，不同就报口令错。
//
// 比对走常数时间，不因前几个字节相同而提早返回。
func (c *Cipher) VerifyCheckBlock(stored []byte) error {
	if len(stored) != CheckSize {
		return xerr.InvalidDatabase.Newf("password check block must be %d bytes, got %d", CheckSize, len(stored))
	}
	if subtle.ConstantTimeCompare(stored, c.check[:]) != 1 {
		return xerr.InvalidPassword.New("wrong password")
	}
	return nil
}

// BlankBaseline 返回空洞基准块，见 [Cipher] 的 blank 字段。
func (c *Cipher) BlankBaseline() [BlockSize]byte { return c.blank }

// String 只报算法与密钥位数，不泄露密钥本身。
func (c *Cipher) String() string { return fmt.Sprintf("xcrypt.Cipher(AES-%d)", KeySize*8) }
