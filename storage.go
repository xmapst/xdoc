package xdoc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xengine"
)

const (
	// DefaultFilesCollection 是默认存放文件描述的集合名。
	DefaultFilesCollection = "_files"

	// DefaultChunksCollection 是默认存放文件数据块的集合名。
	DefaultChunksCollection = "_chunks"
)

// ChunkSize 是每个数据块的字节数。
//
// 比 256 KiB 少 1 KiB：留出主键、块号与文档头的空间，让一个块连同外壳
// 仍落在整数个页里。
const ChunkSize = 255 * 1024

const (
	// 文件描述文档的字段名。
	fileFieldFilename   = "filename"
	fileFieldMimeType   = "mimeType"
	fileFieldLength     = "length"
	fileFieldChunks     = "chunks"
	fileFieldUploadDate = "uploadDate"
	fileFieldMetadata   = "metadata"
)

const (
	// 数据块文档的字段名。
	//
	// 前两个是主键里的键名，短是为了省字节：每个块都要带一份。
	chunkFieldFile  = "f"
	chunkFieldIndex = "n"
	chunkFieldData  = "data"
)

// chunkDeleteBatch 是一次删多少个块。
const chunkDeleteBatch = 256

// ErrFileNotFound 表示按 ID 找不到这个文件。
var ErrFileNotFound = errors.New("xdoc: file not found")

// Storage 把大块二进制内容切成块存进两个集合。
//
// 分块是因为单篇文档有大小上限；描述与数据分开，则让列目录不必读出文件内容。
type Storage struct {
	files  *Collection
	chunks *Collection
}

// Storage 返回用默认集合名的文件存储。
func (db *DB) Storage() *Storage {
	return db.StorageOn(DefaultFilesCollection, DefaultChunksCollection)
}

// StorageOn 返回用指定集合名的文件存储。
//
// 同一个库里可以开好几套互不相干的存储。
func (db *DB) StorageOn(files, chunks string) *Storage {
	return &Storage{files: db.Collection(files), chunks: db.Collection(chunks)}
}

// Files 返回存放文件描述的集合，可以直接对它查询或建索引。
func (s *Storage) Files() *Collection { return s.files }

// Chunks 返回存放数据块的集合。
func (s *Storage) Chunks() *Collection { return s.chunks }

// FileInfo 是一个文件的描述，不含内容。
type FileInfo struct {
	// ID 是文件的主键，由调用方指定。
	ID *Value

	// Filename 是文件名，已经去掉目录部分。
	Filename string

	// MimeType 由文件扩展名推出。
	MimeType string

	// Length 是文件字节数。
	Length int64

	// Chunks 是数据块个数。
	Chunks int32

	// UploadDate 是最后一次写完的时刻。
	UploadDate time.Time

	// Metadata 是调用方附带的任意信息，永远非 nil。
	Metadata *Document
}

// ParseFileInfo 把一篇文件描述文档解成 [FileInfo]。
//
// 缺字段或类型不对时取零值而不报错：这些文档可能被外部改过，
// 一个字段坏掉不该让整份目录读不出来。
func ParseFileInfo(d *Document) *FileInfo {
	if d == nil {
		return nil
	}
	f := &FileInfo{ID: d.Get(xengine.IDField), Metadata: xbson.NewDocument()}
	f.Filename, _ = d.Get(fileFieldFilename).AsString()
	f.MimeType, _ = d.Get(fileFieldMimeType).AsString()
	f.Length = asInt64(d.Get(fileFieldLength))
	f.Chunks = int32(asInt64(d.Get(fileFieldChunks)))
	if t, ok := d.Get(fileFieldUploadDate).AsTime(); ok {
		f.UploadDate = t
	}
	if m, ok := d.Get(fileFieldMetadata).AsDocument(); ok && m != nil {
		f.Metadata = m
	}
	return f
}

// asInt64 把整数或浮点值取成 int64，取不出来时给 0。
func asInt64(v *Value) int64 {
	if v == nil {
		return 0
	}
	if n, ok := v.AsInt64(); ok {
		return n
	}
	if n, ok := v.AsInt32(); ok {
		return int64(n)
	}
	if f, ok := v.AsDouble(); ok {
		return int64(f)
	}
	return 0
}

// document 把 [FileInfo] 转回文档。
//
// 空文件名写成 Null 而不是空串。
func (f *FileInfo) document() (*Document, error) {
	d := xbson.NewDocument()
	d.Set(xengine.IDField, f.ID)

	d.Set(fileFieldFilename, emptyToNull(f.Filename))
	d.Set(fileFieldMimeType, xbson.String(f.MimeType))
	d.Set(fileFieldLength, xbson.Int64(f.Length))
	d.Set(fileFieldChunks, xbson.Int32(f.Chunks))
	ts, err := xbson.DateTime(f.UploadDate)
	if err != nil {
		return nil, fmt.Errorf("xdoc: upload date: %w", err)
	}
	d.Set(fileFieldUploadDate, ts)
	meta := f.Metadata
	if meta == nil {
		meta = xbson.NewDocument()
	}
	d.Set(fileFieldMetadata, meta.Value())
	return d, nil
}

// emptyToNull 把空串转成 Null。
func emptyToNull(s string) *Value {
	if s == "" {
		return xbson.Null
	}
	return xbson.String(s)
}

// chunkID 拼出第 n 块的主键：{f: 文件ID, n: 块号}。
//
// 主键本身就带序，同一文件的块因此在主键索引上连续排列，顺序读一路走下去
// 不必来回跳。
func chunkID(fileID *Value, n int32) *Value {
	d := xbson.NewDocument()
	d.Set(chunkFieldFile, fileID)
	d.Set(chunkFieldIndex, xbson.Int32(n))
	return d.Value()
}

// baseName 去掉路径部分，两种分隔符都认。
//
// 不用 [path/filepath.Base]：那个跟着运行平台走，同一个上传路径在两个系统上
// 会存成不同的文件名。
func baseName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		return name[i+1:]
	}
	return name
}

// FindByID 按 ID 取文件描述，没有则返回 [ErrFileNotFound]。
func (s *Storage) FindByID(ctx context.Context, id *Value) (*FileInfo, error) {
	d, err := s.files.FindByID(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrFileNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return ParseFileInfo(d), nil
}

// Exists 报告某个 ID 的文件在不在。
func (s *Storage) Exists(ctx context.Context, id *Value) (bool, error) {
	return s.files.Exists(ctx, id)
}

// FindAll 按 ID 顺序遍历全部文件描述。
func (s *Storage) FindAll(ctx context.Context) iter.Seq2[*FileInfo, error] {
	return infoSeq(s.files.All(ctx, Asc))
}

// Find 按条件遍历文件描述，条件里用 @0、@1 引用 args。
//
// 条件为空时报错而不是"匹配全部"：一个不小心传空的条件会静默变成全表，
// 用 [Storage.FindAll] 表达那个意思。
func (s *Storage) Find(ctx context.Context, where string, args ...any) iter.Seq2[*FileInfo, error] {
	if strings.TrimSpace(where) == "" {
		err := errors.New("xdoc: storage: find expression is required")
		return func(yield func(*FileInfo, error) bool) { yield(nil, err) }
	}
	q := s.files.Query().Where(where)
	for i, a := range args {
		q = q.Param(strconv.Itoa(i), a)
	}
	return infoSeq(q.All(ctx))
}

// infoSeq 把文档序列逐篇转成 [FileInfo] 序列。
func infoSeq(src iter.Seq2[*Document, error]) iter.Seq2[*FileInfo, error] {
	return func(yield func(*FileInfo, error) bool) {
		for d, err := range src {
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(ParseFileInfo(d), nil) {
				return
			}
		}
	}
}

// SetMetadata 换掉一个文件的附带信息，文件不存在时返回 false 而不报错。
//
// 是整份替换，不是合并。
func (s *Storage) SetMetadata(ctx context.Context, id *Value, metadata *Document) (bool, error) {
	info, err := s.FindByID(ctx, id)
	if errors.Is(err, ErrFileNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if metadata == nil {
		metadata = xbson.NewDocument()
	}
	info.Metadata = metadata
	d, err := info.document()
	if err != nil {
		return false, err
	}
	n, err := s.files.Update(ctx, d)
	return n > 0, err
}

// Delete 删掉一个文件与它的全部数据块。
//
// 先删描述再删块：中途失败留下的是一堆没人引用的块，而不是一个读到一半
// 就报缺块的文件。
func (s *Storage) Delete(ctx context.Context, id *Value) (bool, error) {
	info, err := s.FindByID(ctx, id)
	if errors.Is(err, ErrFileNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := s.files.Delete(ctx, id); err != nil {
		return false, err
	}
	return true, s.deleteChunks(ctx, id, info.Chunks)
}

// deleteChunks 删掉一个文件的数据块。
//
// 删完已知的 count 块之后**继续往后试**，直到某一批一个都没删着：
// 文件被覆盖成更短的一份时，上一次留下的尾巴就靠这一段清掉。
func (s *Storage) deleteChunks(ctx context.Context, fileID *Value, count int32) error {
	ids := make([]*Value, 0, chunkDeleteBatch)
	next := int32(0)

	for next < count {
		ids = ids[:0]
		for range chunkDeleteBatch {
			if next >= count {
				break
			}
			ids = append(ids, chunkID(fileID, next))
			next++
		}
		if _, err := s.chunks.Delete(ctx, ids...); err != nil {
			return err
		}
	}

	for {
		ids = ids[:0]
		for range chunkDeleteBatch {
			ids = append(ids, chunkID(fileID, next))
			next++
		}
		n, err := s.chunks.Delete(ctx, ids...)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

// OpenWrite 开始写一个文件，同 ID 的旧内容先删干净。
//
// ID 与文件名都不能空。metadata 为 nil 时保留原有的那份。
//
// 写入过程**不在一个事务里**：块是一边收一边插的。中途放弃会在库里留下
// 一批孤块，下一次写同一个 ID 会把它们清掉。要整体成败一致，
// 自己开一个事务把整个写入包起来。
func (s *Storage) OpenWrite(ctx context.Context, id *Value, filename string, metadata *Document) (*FileWriter, error) {
	if id == nil || id.IsNull() {
		return nil, errors.New("xdoc: storage: file id is required")
	}

	if filename == "" {
		return nil, errors.New("xdoc: storage: filename is required")
	}
	name := baseName(filename)
	info, err := s.FindByID(ctx, id)
	switch {
	case errors.Is(err, ErrFileNotFound):
		info = &FileInfo{ID: id, Metadata: xbson.NewDocument()}
	case err != nil:
		return nil, err
	}
	info.Filename = name
	info.MimeType = mimeTypeOf(name)
	if metadata != nil {
		info.Metadata = metadata
	}
	if info.Metadata == nil {
		info.Metadata = xbson.NewDocument()
	}

	if info.Chunks > 0 || info.Length > 0 {
		if err := s.deleteChunks(ctx, id, info.Chunks); err != nil {
			return nil, err
		}
	}
	info.Length = 0
	info.Chunks = 0
	return &FileWriter{
		st:   s,
		ctx:  ctx,
		info: info,
		buf:  make([]byte, 0, ChunkSize),
	}, nil
}

// FileWriter 往一个文件里写内容，攒满一块才落库。
//
// 它记住第一个错误，之后每次调用都直接返回同一个错误——半截的内容不该被
// 接着往下写。
type FileWriter struct {
	st     *Storage
	ctx    context.Context
	info   *FileInfo
	buf    []byte
	closed bool
	err    error
}

// FileInfo 返回当前的文件描述。
//
// 长度与块数是边写边涨的，写完才是最终值。
func (w *FileWriter) FileInfo() *FileInfo { return w.info }

// Write 写入一段字节，攒满一块就落库。
func (w *FileWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.closed {
		return 0, errors.New("xdoc: storage: write after close")
	}
	total := len(p)
	for len(p) > 0 {
		n := min(ChunkSize-len(w.buf), len(p))
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		if len(w.buf) == ChunkSize {
			if err := w.flushChunk(); err != nil {
				w.err = err
				return total - len(p), err
			}
		}
	}
	return total, nil
}

// ReadFrom 把 r 读到尽头。
//
// 直接读进块缓冲，中间不再多一层拷贝。
func (w *FileWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.closed {
		return 0, errors.New("xdoc: storage: write after close")
	}
	var total int64
	for {
		n, err := r.Read(w.buf[len(w.buf):ChunkSize])
		w.buf = w.buf[:len(w.buf)+n]
		total += int64(n)
		if len(w.buf) == ChunkSize {
			if ferr := w.flushChunk(); ferr != nil {
				w.err = ferr
				return total, ferr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
	}
}

// flushChunk 把攒着的字节写成一个块，缓冲空着时什么也不做。
func (w *FileWriter) flushChunk() error {
	if len(w.buf) == 0 {
		return nil
	}
	doc := xbson.NewDocument()
	doc.Set(xengine.IDField, chunkID(w.info.ID, w.info.Chunks))

	doc.Set(chunkFieldData, xbson.Binary(w.buf))
	if _, err := w.st.chunks.Insert(w.ctx, doc); err != nil {
		return err
	}
	w.info.Chunks++
	w.info.Length += int64(len(w.buf))
	w.buf = w.buf[:0]
	return nil
}

// Flush 把攒着的字节落库并更新文件描述，之后还能接着写。
//
// 不满一块也照写：中途落一个短块之后再接着写，会让后面的块大小不一，
// 但读的一侧按每块自己的长度走，所以读得回来。
func (w *FileWriter) Flush() error {
	if w.err != nil {
		return w.err
	}
	if w.closed {
		return errors.New("xdoc: storage: flush after close")
	}
	if err := w.flushChunk(); err != nil {
		w.err = err
		return err
	}
	if err := w.writeInfo(); err != nil {
		w.err = err
		return err
	}
	return nil
}

// writeInfo 写入文件描述，上传时刻取当下。
func (w *FileWriter) writeInfo() error {
	w.info.UploadDate = time.Now()
	d, err := w.info.document()
	if err != nil {
		return err
	}
	_, err = w.st.files.Upsert(w.ctx, d)
	return err
}

// Close 落下最后一块并写入文件描述。
//
// **不写完不算数**：没有 Close（或 Flush）过的文件，描述里的长度与块数
// 还是上一次的值。
//
// 重复调用返回同一个结果，不会重复写。
func (w *FileWriter) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	if w.err != nil {
		return w.err
	}
	if err := w.flushChunk(); err != nil {
		w.err = err
		return err
	}
	if err := w.writeInfo(); err != nil {
		w.err = err
		return err
	}
	return nil
}

// Upload 把 r 的全部内容存成一个文件。
//
// 读出错时不写文件描述：库里会留下一批孤块，但目录里不会出现一个
// 长度对不上的文件。
func (s *Storage) Upload(ctx context.Context, id *Value, filename string, r io.Reader, metadata *Document) (*FileInfo, error) {
	if r == nil {
		return nil, errors.New("xdoc: storage: reader is nil")
	}
	w, err := s.OpenWrite(ctx, id, filename, metadata)
	if err != nil {
		return nil, err
	}
	if _, err := w.ReadFrom(r); err != nil {
		w.closed = true
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return w.info, nil
}

// UploadFile 把磁盘上的一份文件存进来，文件名取路径的最后一段。
func (s *Storage) UploadFile(ctx context.Context, id *Value, path string) (*FileInfo, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("xdoc: storage: filename is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return s.Upload(ctx, id, filepath.Base(path), f, nil)
}

// OpenRead 打开一个文件来读，支持随机定位。
func (s *Storage) OpenRead(ctx context.Context, id *Value) (*FileReader, error) {
	info, err := s.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return &FileReader{st: s, ctx: ctx, info: info, starts: []int64{0}}, nil
}

// FileReader 顺序或随机地读出一个文件的内容。
type FileReader struct {
	st   *Storage
	ctx  context.Context
	info *FileInfo

	// idx 是当前块号，cur 是它的内容，pos 是块内位置，off 是文件内位置。
	idx int32
	cur []byte
	pos int
	off int64

	// starts 是已知各块的起始偏移，starts[i] 是第 i 块的开头。
	//
	// 块大小不保证一致（[FileWriter.Flush] 会落出短块），所以偏移只能一块一块
	// 量出来，边读边记。定位时靠它跳过前面已经量过的部分。
	starts []int64
}

// FileInfo 返回这个文件的描述。
func (r *FileReader) FileInfo() *FileInfo { return r.info }

// Read 读出接下来的字节，读到末尾返回 [io.EOF]。
func (r *FileReader) Read(p []byte) (int, error) {
	if r.off >= r.info.Length {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && r.off < r.info.Length {
		if r.cur == nil || r.pos >= len(r.cur) {
			if err := r.loadChunk(r.idx); err != nil {
				return n, err
			}
		}
		c := copy(p[n:], r.cur[r.pos:])
		n += c
		r.pos += c
		r.off += int64(c)
		if r.pos >= len(r.cur) {
			r.idx++
			r.cur = nil
			r.pos = 0
		}
	}
	return n, nil
}

// WriteTo 把剩下的内容全部写进 w。
//
// 按块转发，中间不攒整份内容。写出的字节数以描述里的长度为准：
// 最后一块可能比实际内容长，多出来的那截不写。
func (r *FileReader) WriteTo(w io.Writer) (int64, error) {
	var total int64
	for r.off < r.info.Length {
		if r.cur == nil || r.pos >= len(r.cur) {
			if err := r.loadChunk(r.idx); err != nil {
				return total, err
			}
		}

		end := len(r.cur)
		if rest := r.info.Length - r.off; int64(end-r.pos) > rest {
			end = r.pos + int(rest)
		}
		n, err := w.Write(r.cur[r.pos:end])
		r.pos += n
		r.off += int64(n)
		total += int64(n)
		if err != nil {
			return total, err
		}
		if r.pos >= len(r.cur) {
			r.idx++
			r.cur = nil
			r.pos = 0
		}
	}
	return total, nil
}

// loadChunk 读进第 i 块。
//
// 块缺失时的错误里带上块号与总块数——那说明库里的块被删过或没写完。
func (r *FileReader) loadChunk(i int32) error {
	d, err := r.st.chunks.FindByID(r.ctx, chunkID(r.info.ID, i))
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("xdoc: storage: file %s is missing chunk %d of %d", r.info.ID, i, r.info.Chunks)
	}
	if err != nil {
		return err
	}
	data, _ := d.Get(chunkFieldData).AsBinary()
	r.cur = data
	r.pos = 0
	r.idx = i

	if int(i)+1 == len(r.starts) {
		r.starts = append(r.starts, r.starts[i]+int64(len(data)))
	}
	return nil
}

// Seek 定位到文件内某个位置。
//
// 定位到末尾之后是允许的，之后读只会得到 [io.EOF]。
//
// 块大小不保证一致，所以往前定位要从已知的最近一块起逐块读过去；
// 读过的块起点记在 starts 里，重复定位到同一片区域不必再量一遍。
func (r *FileReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.off + offset
	case io.SeekEnd:
		abs = r.info.Length + offset
	default:
		return 0, fmt.Errorf("xdoc: storage: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, errors.New("xdoc: storage: negative position")
	}
	if abs >= r.info.Length {
		r.off = abs
		r.cur = nil
		r.pos = 0
		r.idx = r.info.Chunks
		return abs, nil
	}

	i := int32(0)
	for int(i)+1 < len(r.starts) && r.starts[i+1] <= abs {
		i++
	}
	for {
		if err := r.loadChunk(i); err != nil {
			return 0, err
		}
		end := r.starts[i] + int64(len(r.cur))
		if abs < end || len(r.cur) == 0 {
			r.pos = int(abs - r.starts[i])
			r.off = abs
			return abs, nil
		}
		i++
	}
}

// Close 放掉当前块占的内存。
//
// 没有别的资源要还，所以它从不出错，之后也还能接着定位与读。
func (r *FileReader) Close() error {
	r.cur = nil
	return nil
}

// Download 把一个文件的全部内容写进 w。
func (s *Storage) Download(ctx context.Context, id *Value, w io.Writer) (*FileInfo, error) {
	if w == nil {
		return nil, errors.New("xdoc: storage: writer is nil")
	}
	r, err := s.OpenRead(ctx, id)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if _, err := r.WriteTo(w); err != nil {
		return nil, err
	}
	return r.info, nil
}

// DownloadFile 把一个文件写到磁盘上。
//
// 先确认文件在库里再建目标文件：不然一个打错的 ID 会留下一个空文件，
// overwrite 为真时甚至会先把原来那份截空。
//
// 覆盖时是真截断，不会留下旧内容的尾巴。
func (s *Storage) DownloadFile(ctx context.Context, id *Value, path string, overwrite bool) (*FileInfo, error) {
	if _, err := s.FindByID(ctx, id); err != nil {
		return nil, err
	}
	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !overwrite {
		flag = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := s.Download(ctx, id, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	return info, nil
}
