package xdoc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/xmapst/xdoc/internal/xcrypt"
	"github.com/xmapst/xdoc/internal/xpage"
)

// Backup 把整个库此刻已提交的内容写成一份数据文件交给 w，返回写了多少字节。
//
// 写出来的与刚做完检查点的数据文件一样：不要日志，直接 [Open] 即可。加密库写出的是
// 同一个口令加密的文件；内存库也能这样落成文件；只读句柄与共享连接同样能备份。
//
// 备份定住开始那一刻的版本，写入不必停：期间的提交照常进行，只是不进这份备份。
// 检查点要等备份结束才能做，所以这段时间日志只涨不缩。在途事务的改动不在备份里，
// 它们刚占下的页在备份里可能是没挂进空页链的空页，只占空间，重建时回收。
//
// ctx 取消时返回 ctx.Err()，w 里已经写下的是半截。要落成文件用 [DB.BackupFile]。
func (db *DB) Backup(ctx context.Context, w io.Writer) (int64, error) {
	rel, err := db.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer rel()

	out := &backupWriter{w: w}
	if db.password != "" {
		if err := out.encrypt(db.password); err != nil {
			return out.n, fmt.Errorf("xdoc: backup: %w", err)
		}
	}
	if err := db.core.Backup(ctx, out.page); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return out.n, cerr
		}
		return out.n, fmt.Errorf("xdoc: backup: %w", err)
	}
	return out.n, nil
}

// BackupFile 把 [DB.Backup] 的结果落成 path 这个新文件。
//
// path 已经存在时报错（[fs.ErrExist]），不覆盖。先写进同目录下的临时文件并刷到设备，
// 再换名成 path：path 要么没有，要么是一份完整的备份。中途失败或者 ctx 取消都会把
// 临时文件删掉，取消时返回 ctx.Err()。
func (db *DB) BackupFile(ctx context.Context, path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("xdoc: backup to %q: %w", path, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("xdoc: backup to %q: %w", path, err)
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("xdoc: backup to %q: %w", path, err)
	}
	tmp := f.Name()
	// 临时文件默认只有属主可读，改成与新建库文件一样。
	_ = f.Chmod(0o644)

	bw := bufio.NewWriterSize(f, 64*xpage.PageSize)
	_, err = db.Backup(ctx, bw)
	if err == nil {
		err = wrapBackupPath(path, errors.Join(bw.Flush(), f.Sync()))
	}
	if cerr := f.Close(); err == nil {
		err = wrapBackupPath(path, cerr)
	}
	if err == nil {
		err = placeBackup(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	syncDir(dir)
	return nil
}

// wrapBackupPath 给落文件这一步的错误带上目标路径，nil 原样返回。
func wrapBackupPath(path string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("xdoc: backup to %q: %w", path, err)
}

// placeBackup 把写好的临时文件放到 path，path 已经存在时报错而不覆盖。
//
// 先试硬链接：目标存在时它直接失败，检查与落位是同一步，两个备份抢同一个路径不会互相覆盖。
// 文件系统不支持硬链接时退回先看一眼再换名，两步之间别人抢先建出同名文件的话会被覆盖。
func placeBackup(tmp, path string) error {
	err := os.Link(tmp, path)
	if err == nil {
		_ = os.Remove(tmp)
		return nil
	}
	if _, serr := os.Lstat(path); errors.Is(err, fs.ErrExist) || serr == nil {
		return fmt.Errorf("xdoc: backup to %q: %w", path, fs.ErrExist)
	}
	return wrapBackupPath(path, os.Rename(tmp, path))
}

// backupWriter 把备份的页写给 w，数着写了多少字节。设了口令时先写盐页，之后每页加密再写。
type backupWriter struct {
	w io.Writer
	n int64

	cipher *xcrypt.Cipher
	buf    []byte
}

// encrypt 取一份新盐派生密钥，写出盐页：加密标志、盐、口令校验块，与新建加密库时一样。
func (b *backupWriter) encrypt(password string) error {
	salt, err := xcrypt.NewSalt()
	if err != nil {
		return err
	}
	c, err := xcrypt.NewCipher(password, salt[:])
	if err != nil {
		return err
	}
	head := make([]byte, xcrypt.PageSize)
	head[xcrypt.FlagOffset] = xcrypt.FlagEncrypted
	copy(head[xcrypt.SaltOffset:], salt[:])
	check := c.CheckBlock()
	copy(head[xcrypt.CheckOffset:], check[:])
	if err := b.write(head); err != nil {
		return err
	}
	b.cipher, b.buf = c, make([]byte, xpage.PageSize)
	return nil
}

// page 写出一页，设了口令时先加密到自己的缓冲里——传进来的页不能改。
func (b *backupWriter) page(p []byte) error {
	if b.cipher != nil {
		if err := b.cipher.Encrypt(b.buf, p); err != nil {
			return err
		}
		p = b.buf
	}
	return b.write(p)
}

// write 把 p 整段写给 w，写不满算错。
func (b *backupWriter) write(p []byte) error {
	n, err := b.w.Write(p)
	b.n += int64(n)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	return err
}
