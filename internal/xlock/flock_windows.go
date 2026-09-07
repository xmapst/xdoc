//go:build windows

package xlock

import (
	"os"
	"syscall"
	"unsafe"
)

// platform 是本平台加锁用的接口名，出现在错误消息里。
const platform = "LockFileEx"

var (
	// 系统加锁接口，按需载入。
	kernel32     = syscall.NewLazyDLL("kernel32.dll")
	procLockFile = kernel32.NewProc("LockFileEx")
	procUnlock   = kernel32.NewProc("UnlockFileEx")
)

const (
	// 加锁标志：独占、不等待。
	lockfileExclusive    = 0x00000002
	lockfileFailImmediat = 0x00000001

	// errLockViolation 是「锁被别人占着」对应的系统错误号。
	errLockViolation syscall.Errno = 33
)

// tryLock 非阻塞地取一把独占文件锁。
//
// 只锁第一个字节：要的是一个互斥标记。
func tryLock(f *os.File) error {
	var ov syscall.Overlapped
	r, _, err := procLockFile.Call(
		uintptr(syscall.Handle(f.Fd())),
		uintptr(lockfileExclusive|lockfileFailImmediat),
		0, 1, 0,
		uintptr(unsafe.Pointer(&ov)),
	)
	if r != 0 {
		return nil
	}
	if err == errLockViolation {
		return errWouldBlock
	}
	return err
}

// unlock 解开文件锁。
func unlock(f *os.File) error {
	var ov syscall.Overlapped
	r, _, err := procUnlock.Call(
		uintptr(syscall.Handle(f.Fd())),
		0, 1, 0,
		uintptr(unsafe.Pointer(&ov)),
	)
	if r != 0 {
		return nil
	}
	return err
}
