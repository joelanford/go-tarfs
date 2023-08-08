package tarfs

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

const (
	blockSize = 512 // Size of each block in a tar stream
)

type tarfs struct {
	entries map[string]fs.DirEntry
}

// New creates a new tar fs.FS from r
func New(r io.Reader) (fs.FS, error) {
	tfs := &tarfs{make(map[string]fs.DirEntry)}
	tfs.entries["."] = newDirEntry(fs.FileInfoToDirEntry(fakeDirFileInfo(".")))

	ra, isReaderAt := r.(readReaderAt)
	if !isReaderAt {
		buf, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		ra = bytes.NewReader(buf)
	}

	var cr readCounterIface
	if rs, isReadSeeker := ra.(io.ReadSeeker); isReadSeeker {
		cr = &readSeekCounter{ReadSeeker: rs}
	} else {
		cr = &readCounter{Reader: ra}
	}

	tr := tar.NewReader(cr)

	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		name := path.Clean(h.Name)
		if name == "." {
			continue
		}

		de := fs.FileInfoToDirEntry(h.FileInfo())

		if h.FileInfo().IsDir() {
			tfs.append(name, newDirEntry(de))
		} else {
			tfs.append(name, &regEntry{de, name, ra, cr.Count() - blockSize})
		}
	}

	return tfs, nil
}

type readReaderAt interface {
	io.Reader
	io.ReaderAt
}

type readCounterIface interface {
	io.Reader
	Count() int64
}

type readCounter struct {
	io.Reader
	off int64
}

func (cr *readCounter) Read(p []byte) (n int, err error) {
	n, err = cr.Reader.Read(p)
	cr.off += int64(n)
	return
}

func (cr *readCounter) Count() int64 {
	return cr.off
}

type readSeekCounter struct {
	io.ReadSeeker
	off int64
}

func (cr *readSeekCounter) Read(p []byte) (n int, err error) {
	n, err = cr.ReadSeeker.Read(p)
	cr.off += int64(n)
	return
}

func (cr *readSeekCounter) Seek(offset int64, whence int) (abs int64, err error) {
	abs, err = cr.ReadSeeker.Seek(offset, whence)
	cr.off = abs
	return
}

func (cr *readSeekCounter) Count() int64 {
	return cr.off
}

func (tfs *tarfs) append(name string, e fs.DirEntry) {
	tfs.entries[name] = e

	dir := path.Dir(name)

	if parent, ok := tfs.entries[dir]; ok {
		parent := parent.(*dirEntry)
		parent.append(e)
		return
	}

	parent := newDirEntry(fs.FileInfoToDirEntry(fakeDirFileInfo(path.Base(dir))))

	tfs.append(dir, parent)

	parent.append(e)
}

var _ fs.FS = &tarfs{}

func (tfs *tarfs) Open(name string) (fs.File, error) {
	const op = "open"

	e, err := tfs.get(op, name)
	if err != nil {
		return nil, err
	}

	return e.open()
}

var _ fs.ReadDirFS = &tarfs{}

func (tfs *tarfs) ReadDir(name string) ([]fs.DirEntry, error) {
	e, err := tfs.get("readdir", name)
	if err != nil {
		return nil, err
	}

	return e.readdir(name)
}

var _ fs.ReadFileFS = &tarfs{}

func (tfs *tarfs) ReadFile(name string) ([]byte, error) {
	e, err := tfs.get("readfile", name)
	if err != nil {
		return nil, err
	}

	return e.readfile(name)
}

var _ fs.StatFS = &tarfs{}

func (tfs *tarfs) Stat(name string) (fs.FileInfo, error) {
	e, err := tfs.get("stat", name)
	if err != nil {
		return nil, err
	}

	return e.Info()
}

var _ fs.GlobFS = &tarfs{}

func (tfs *tarfs) Glob(pattern string) (matches []string, _ error) {
	for name := range tfs.entries {
		match, err := path.Match(pattern, name)
		if err != nil {
			return nil, err
		}
		if match {
			matches = append(matches, name)
		}
	}
	return
}

var _ fs.SubFS = &tarfs{}

func (tfs *tarfs) Sub(dir string) (fs.FS, error) {
	const op = "sub"

	if dir == "." {
		return tfs, nil
	}

	e, err := tfs.get(op, dir)
	if err != nil {
		return nil, err
	}

	subfs := &tarfs{make(map[string]fs.DirEntry)}

	subfs.entries["."] = e

	prefix := dir + "/"
	for name, file := range tfs.entries {
		if strings.HasPrefix(name, prefix) {
			subfs.entries[strings.TrimPrefix(name, prefix)] = file
		}
	}

	return subfs, nil
}

func (tfs *tarfs) get(op, path string) (entry, error) {
	if !fs.ValidPath(path) {
		return nil, newErr(op, path, fs.ErrInvalid)
	}

	e, ok := tfs.entries[path]
	if !ok {
		return nil, newErrNotExist(op, path)
	}

	return e.(entry), nil
}

type entry interface {
	fs.DirEntry
	size() int64
	readdir(path string) ([]fs.DirEntry, error)
	readfile(path string) ([]byte, error)
	entries(op, path string) ([]fs.DirEntry, error)
	open() (fs.File, error)
}

type regEntry struct {
	fs.DirEntry
	name   string
	ra     io.ReaderAt
	offset int64
}

var _ entry = &regEntry{}

func (e *regEntry) size() int64 {
	info, _ := e.Info() // err is necessarily nil
	return info.Size()
}

func (e *regEntry) readdir(path string) ([]fs.DirEntry, error) {
	return nil, newErrNotDir("readdir", path)
}

func (e *regEntry) readfile(path string) ([]byte, error) {
	r, err := e.reader()
	if err != nil {
		return nil, err
	}

	b := bytes.NewBuffer(make([]byte, 0, e.size()))

	if _, err := io.Copy(b, r); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func (e *regEntry) entries(op, path string) ([]fs.DirEntry, error) {
	return nil, newErrNotDir(op, path)
}

func (e *regEntry) open() (fs.File, error) {
	r, err := e.reader()
	if err != nil {
		return nil, err
	}

	return &file{e, &readSeeker{&readCounter{r, 0}, e}, -1, false}, nil
}

func (e *regEntry) reader() (io.Reader, error) {
	tr := tar.NewReader(io.NewSectionReader(e.ra, e.offset, 1<<63-1))

	if _, err := tr.Next(); err != nil {
		return nil, err
	}

	return tr, nil
}

type dirEntry struct {
	fs.DirEntry
	_entries []fs.DirEntry
	sorted   bool
}

func newDirEntry(e fs.DirEntry) *dirEntry {
	return &dirEntry{e, make([]fs.DirEntry, 0, 10), false}
}

func (e *dirEntry) append(c fs.DirEntry) {
	e._entries = append(e._entries, c)
}

var _ entry = &dirEntry{}

func (e *dirEntry) size() int64 {
	return 0
}

func (e *dirEntry) readdir(path string) ([]fs.DirEntry, error) {
	if !e.sorted {
		sort.Sort(entriesByName(e._entries))
	}

	entries := make([]fs.DirEntry, len(e._entries))

	copy(entries, e._entries)

	return entries, nil
}

func (e *dirEntry) readfile(path string) ([]byte, error) {
	return nil, newErrDir("readfile", path)
}

func (e *dirEntry) entries(op, path string) ([]fs.DirEntry, error) {
	if !e.sorted {
		sort.Sort(entriesByName(e._entries))
	}

	return e._entries, nil
}

func (e *dirEntry) open() (fs.File, error) {
	return &file{e, nil, 0, false}, nil
}

type fakeDirFileInfo string

var _ fs.FileInfo = fakeDirFileInfo("")

func (e fakeDirFileInfo) Name() string {
	return string(e)
}

func (fakeDirFileInfo) Size() int64 {
	return 0
}

func (fakeDirFileInfo) Mode() fs.FileMode {
	return fs.ModeDir
}

func (fakeDirFileInfo) ModTime() time.Time {
	return time.Time{}
}

func (fakeDirFileInfo) IsDir() bool {
	return true
}

func (fakeDirFileInfo) Sys() interface{} {
	return nil
}

type entriesByName []fs.DirEntry

var _ sort.Interface = entriesByName{}

func (entries entriesByName) Less(i, j int) bool {
	return entries[i].Name() < entries[j].Name()
}

func (entries entriesByName) Len() int {
	return len(entries)
}

func (entries entriesByName) Swap(i, j int) {
	entries[i], entries[j] = entries[j], entries[i]
}

type readSeeker struct {
	*readCounter
	e *regEntry
}

var _ io.ReadSeeker = &readSeeker{}

func (rs *readSeeker) Seek(offset int64, whence int) (int64, error) {
	const op = "seek"

	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = rs.off + offset
	case io.SeekEnd:
		abs = rs.e.size() + offset
	default:
		return 0, newErr(op, rs.e.name, errors.New("invalid whence"))
	}
	if abs < 0 {
		return 0, newErr(op, rs.e.name, errors.New("negative position"))
	}

	if abs < rs.off {
		r, err := rs.e.reader()
		if err != nil {
			return 0, err
		}

		rs.readCounter = &readCounter{r, 0}
	}

	if abs > rs.off {
		if _, err := io.CopyN(io.Discard, rs.readCounter, abs-rs.off); err != nil && err != io.EOF {
			return 0, err
		}
	}

	return abs, nil
}
