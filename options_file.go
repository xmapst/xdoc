package xdoc

import (
	"errors"
	"os"
	"path/filepath"
)

// errFileAccessDisabled 表示这个库句柄上的 $file 被 [WithoutFileAccess] 关掉了。
var errFileAccessDisabled = errors.New("xdoc: $file is disabled by WithoutFileAccess")

// fileAccessMode 说明 $file 能碰哪些文件。
type fileAccessMode uint8

const (
	// fileAccessRoot 是默认：只能碰根目录之下的文件。
	fileAccessRoot fileAccessMode = iota

	// fileAccessUnrestricted 不设限，文件名原样交给操作系统。
	fileAccessUnrestricted

	// fileAccessDisabled 完全不许读写文件。
	fileAccessDisabled
)

// fileAccess 是 $file 的访问范围。
type fileAccess struct {
	mode fileAccessMode

	// root 是限定的根目录；为空表示打开库时的工作目录，打开时解析成绝对路径。
	root string

	// err 是解析根目录时的失败，留到用 $file 时再报——打开库不该因此失败。
	err error
}

// WithFileRoot 把 $file 的读写限定在 dir 之下。
//
// 相对的 dir 按打开库时的工作目录解析。绝对路径、`..` 与符号链接都逃不出去。
// 不给这个选项时，根就是打开库时的工作目录。
func WithFileRoot(dir string) Option {
	return func(o *options) { o.file = fileAccess{mode: fileAccessRoot, root: dir} }
}

// WithUnrestrictedFileAccess 取消 $file 的目录限制，文件名原样交给操作系统。
//
// **SQL 来自不可信来源时不要用**：`overwritten:true` 能覆写进程可写的任意文件。
func WithUnrestrictedFileAccess() Option {
	return func(o *options) { o.file = fileAccess{mode: fileAccessUnrestricted} }
}

// WithoutFileAccess 完全禁用 $file，读写都报错。
func WithoutFileAccess() Option {
	return func(o *options) { o.file = fileAccess{mode: fileAccessDisabled} }
}

// resolve 把根目录定成绝对路径。
//
// 要在打开库时调：之后进程换了工作目录，$file 也不跟着走。
func (fa fileAccess) resolve() fileAccess {
	if fa.mode == fileAccessRoot {
		fa.root, fa.err = filepath.Abs(fa.root)
	}
	return fa
}

// check 在碰文件之前拦住被禁用的情形。
//
// 单独拿出来，是因为一篇都不写的导出根本不会开文件，却同样该报错。
func (fa fileAccess) check() error {
	if fa.mode == fileAccessDisabled {
		return errFileAccessDisabled
	}
	return fa.err
}

// openFile 按访问范围打开文件，签名同 [os.OpenFile]。
//
// 每次现开一个 [os.Root] 再关掉：开出来的文件与它无关，关了照常可用，
// 库句柄因此不必多管一个要关的东西。
func (fa fileAccess) openFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	if err := fa.check(); err != nil {
		return nil, err
	}
	if fa.mode == fileAccessUnrestricted {
		return os.OpenFile(name, flag, perm)
	}
	r, err := os.OpenRoot(fa.root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.OpenFile(name, flag, perm)
}
