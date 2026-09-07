//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package xlock

import (
	"os"
	"syscall"
)

// platform 是本平台加锁用的接口名，出现在错误消息里。
const platform = "flock"

// tryLock 非阻塞地取一把独占文件锁。
//
// 锁被占着时返回 errWouldBlock，其余错误原样上抛。
func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
		return errWouldBlock
	}
	return err
}

// unlock 解开文件锁。
func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
