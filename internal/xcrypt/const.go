// Package xcrypt 给数据文件加一层透明加密：口令派生密钥，按 AES 块就地加解密。
//
// 文件第 0 页是盐页，存加密标志、盐和口令校验块；逻辑偏移 0 对应物理偏移
// [DataOffset]，上层看到的仍是一份从头开始的普通文件。加密不用分组链接模式，
// 每 16 字节独立加解密，因此任意块都能单独读写——这是随机访问页的前提，
// 代价是相同明文块会加出相同密文块。
//
// [Stream] 同时实现 [io.ReadWriteSeeker] 与 ReadAt/WriteAt，前者有位置状态、
// 非并发安全，后者无状态、可并发。
package xcrypt

const (
	// 整个盐页占一页，与数据页同宽。文件的第 0 页就是它，
	// 里面只放加密标志、盐和口令校验块，其余全是零。
	PageSize = 8192

	// FlagOffset 是加密标志所在的字节偏移。
	FlagOffset = 0

	// FlagEncrypted 是加密标志的取值：这个字节是 1 才算加密文件。
	FlagEncrypted = 1

	// 盐紧跟在标志之后，16 字节。
	SaltOffset = 1
	SaltSize   = 16

	// 口令校验块：把 CheckSize 个 CheckPlainByte 用派生出的密钥加密后存这里。
	// 开库时重算一遍再比对，就知道口令对不对——不必先解出任何数据页。
	CheckOffset    = 32
	CheckSize      = 32
	CheckPlainByte = 0x01

	// DataOffset 是逻辑第 0 字节在物理文件里的位置：盐页之后。
	DataOffset = PageSize
)

const (
	// KeySize 是 AES 密钥长度，32 字节即 AES-256。
	KeySize = 32

	// IVSize 是初始向量长度。本实现按块独立加密，并不用 IV，它只参与 DerivedSize 的算术。
	IVSize = 16

	// DerivedSize 是密钥加 IV 的总长。
	DerivedSize = KeySize + IVSize

	// Iterations 是 PBKDF2 的迭代次数。这个数字是文件格式的一部分，改了就打不开旧文件。
	Iterations = 1000

	// BlockSize 是 AES 的分组长度。读写的偏移与长度都必须是它的整数倍。
	BlockSize = 16
)
