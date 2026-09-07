// Package xlock 提供一份库文件的跨进程互斥锁，进程内可重入。
//
// 锁落在临时目录下的一个专用文件上，名字由库文件的绝对路径算出，
// 所以不同路径互不干扰，同一路径的不同写法算出同一把锁。
package xlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// errWouldBlock 表示锁被别人占着，是内部信号，不外传。
var errWouldBlock = errors.New("xlock: would block")

// ErrUnsupported 表示这个平台没有可用的文件锁实现。
var ErrUnsupported = errors.New("xlock: shared connection is not supported on this platform")

// Lock 是一份库的跨进程互斥锁，可重入。
//
// 同一个进程里对同一份库的多个句柄共用一个 entry，所以进程内也是互斥的：
// 只靠文件锁不够——多数平台上同一进程重复加锁不会阻塞。
type Lock struct {
	// entry 是这份库在本进程内共享的那一份锁状态。
	entry *entry
	owner any

	// depth 是本句柄的重入层数，由 mu 守着。
	mu    sync.Mutex
	depth int
}

// entry 是一份库在本进程内唯一的锁状态。
//
// mu 管进程内互斥，f 上的文件锁管进程间互斥。
type entry struct {
	// name 是这份库的锁名，path 是承载文件锁的那个文件。
	name string
	path string

	mu sync.Mutex
	f  *os.File
}

var (
	// registry 按锁名登记全部 entry，保证同名只有一份。
	registryMu sync.Mutex
	registry   = map[string]*entry{}
)

// Open 取一把针对这份库的锁。
//
// 同一份库拿到的是各自独立的 [Lock]（各有各的重入计数），但它们指向同一份
// 锁状态，所以彼此仍然互斥。
//
// 这里不碰文件系统，锁文件要到第一次加锁时才建。
func (s Strategy) Open(dataPath string, owner any) (*Lock, error) {
	name, err := s.Name(dataPath)
	if err != nil {
		return nil, err
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	e, ok := registry[name]
	if !ok {
		e = &entry{name: name, path: lockPath(name)}
		registry[name] = e
	}
	return &Lock{entry: e, owner: owner}, nil
}

// Name 返回锁名。
func (l *Lock) Name() string { return l.entry.name }

// FullName 返回带全局前缀的锁名。
func (l *Lock) FullName() string { return fullName(l.entry.name) }

// Path 返回承载文件锁的那个文件路径。
func (l *Lock) Path() string { return l.entry.path }

// lockPath 拼出锁文件的路径。
//
// 放在临时目录而不是库文件旁边：库所在的目录可能只读，也可能在一个
// 不支持文件锁的网络盘上。
func lockPath(name string) string {
	return filepath.Join(os.TempDir(), ".xdoc", "shm", "global", name+".Mutex")
}

// Acquire 取锁，已经持有则只把重入层数加一。
//
// ctx 取消或超时时放弃等待。
func (l *Lock) Acquire(ctx context.Context) error {
	l.mu.Lock()
	if l.depth > 0 {
		l.depth++
		l.mu.Unlock()
		return nil
	}
	l.mu.Unlock()

	if err := l.entry.lock(ctx); err != nil {
		return err
	}

	l.mu.Lock()
	l.depth = 1
	l.mu.Unlock()
	return nil
}

// Release 放掉一层。降到零时才真正解锁。
//
// 没持有时是空操作，所以可以放心 defer。
func (l *Lock) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.depth == 0 {
		return
	}
	l.depth--
	if l.depth == 0 {
		l.entry.unlock()
	}
}

// Held 报告本句柄此刻是否持有锁。
func (l *Lock) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.depth > 0
}

// lock 依次取进程内互斥与文件锁。
//
// 任何一步失败都要把已经拿到的那部分还回去，否则这份库在本进程内
// 会永远卡住。
func (e *entry) lock(ctx context.Context) error {
	if err := lockCtx(ctx, &e.mu); err != nil {
		return err
	}
	f, err := e.openFile()
	if err != nil {
		e.mu.Unlock()
		return err
	}
	if err := waitLock(ctx, f); err != nil {
		_ = f.Close()
		e.mu.Unlock()
		return err
	}
	e.f = f
	return nil
}

// unlock 解开文件锁、关掉锁文件，再放掉进程内互斥。
func (e *entry) unlock() {
	if e.f != nil {
		_ = unlock(e.f)
		_ = e.f.Close()
		e.f = nil
	}
	e.mu.Unlock()
}

// openFile 建目录并打开锁文件，不存在就建。
func (e *entry) openFile() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(e.path), 0o700); err != nil {
		return nil, fmt.Errorf("xlock: create lock directory: %w", err)
	}
	f, err := os.OpenFile(e.path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("xlock: open lock file %q: %w", e.path, err)
	}
	return f, nil
}

// lockCtx 加一把可被取消的互斥锁。
//
// sync.Mutex 本身不认 ctx，所以另起一个 goroutine 去加锁。取消时那个
// goroutine 还在等——它拿到之后立刻放掉，不然锁就漏了。
func lockCtx(ctx context.Context, mu *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	got := make(chan struct{})
	go func() {
		mu.Lock()
		close(got)
	}()
	select {
	case <-got:
		return nil
	case <-ctx.Done():
		go func() {
			<-got
			mu.Unlock()
		}()
		return ctx.Err()
	}
}

// waitLock 反复尝试加文件锁，直到成功或 ctx 结束。
//
// 只能轮询：多数平台的非阻塞加锁没有「等到就叫我」的接口，而阻塞版本
// 又没法被 ctx 打断。间隔从 1 毫秒翻倍到 32 毫秒封顶——短锁几乎立刻拿到，
// 长锁也不至于空转。
func waitLock(ctx context.Context, f *os.File) error {
	backoff := time.Millisecond
	for {
		switch err := tryLock(f); {
		case err == nil:
			return nil
		case errors.Is(err, errWouldBlock):
		default:
			return fmt.Errorf("xlock: lock %q with %s: %w", f.Name(), platform, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 32*time.Millisecond {
			backoff *= 2
		}
	}
}
