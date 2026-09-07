//go:build aix || solaris || illumos

package xlock

import (
	"os"
	"syscall"
)

// platform 是本平台加锁用的接口名，出现在错误消息里。
const platform = "fcntl"

// tryLock 非阻塞地取一把独占文件锁。
//
// 只锁第一个字节：要的是一个互斥标记，锁整个文件没有额外好处，
// 而且这类锁按字节区间记账，区间越小越省。
func tryLock(f *os.File) error {
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 1}
	err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk)
	if err == syscall.EACCES || err == syscall.EAGAIN {
		return errWouldBlock
	}
	return err
}

// unlock 解开文件锁。
func unlock(f *os.File) error {
	lk := syscall.Flock_t{Type: syscall.F_UNLCK, Whence: 0, Start: 0, Len: 1}
	return syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk)
}
