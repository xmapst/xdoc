//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows || aix || solaris || illumos)

package xlock

import "os"

// platform 是本平台加锁用的接口名，这里没有。
const platform = "none"

// tryLock 在没有文件锁实现的平台上直接报错。
//
// 不能假装成功：那会让多个进程同时以为自己独占，写坏同一份文件。
func tryLock(*os.File) error { return ErrUnsupported }

// unlock 什么也不做——从来就没锁上过。
func unlock(*os.File) error { return nil }
