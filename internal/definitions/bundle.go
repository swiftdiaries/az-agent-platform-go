package definitions

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BundleConfig is the journey configuration file at the root of a Bundle.
const BundleConfig = "journeys.yaml"

const (
	// MaxBundleFiles bounds the number of files in a Bundle.
	MaxBundleFiles = 512
	// MaxBundleBytes bounds the total size of a Bundle.
	MaxBundleBytes = 16 << 20
	// MaxBundleFileBytes bounds the size of one file in a Bundle.
	MaxBundleFileBytes = 1 << 20
)

// Bundle is the in-memory form of a definition set's files: slash-separated
// bundle-relative path to file bytes. The journey configuration is stored
// under BundleConfig.
type Bundle map[string][]byte

// Bundle returns a copy of exactly the files the compile read.
func (s *Set) Bundle() Bundle {
	out := make(Bundle, len(s.bundle))
	for name, data := range s.bundle {
		out[name] = bytes.Clone(data)
	}
	return out
}

func validateBundle(b Bundle) error {
	if len(b) > MaxBundleFiles {
		return errors.New("bundle has too many files")
	}
	total := 0
	for name, data := range b {
		if name == "." || !fs.ValidPath(name) {
			return errors.New("bundle contains an invalid file path")
		}
		if len(data) > MaxBundleFileBytes {
			return fmt.Errorf("bundle file exceeds %d bytes", MaxBundleFileBytes)
		}
		if total += len(data); total > MaxBundleBytes {
			return fmt.Errorf("bundle exceeds %d bytes", MaxBundleBytes)
		}
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if _, clash := b[dir]; clash {
				return errors.New("bundle path is both a file and a directory")
			}
		}
	}
	return nil
}

// ReadBundle reads every regular file below the root of fsys, skipping
// dot-prefixed files and directories. Symbolic links and other non-regular
// files are rejected. BundleConfig is required.
func ReadBundle(fsys fs.FS) (Bundle, error) {
	r := &bundleReader{fsys: fsys, out: Bundle{}}
	if err := r.walk("."); err != nil {
		return nil, err
	}
	if _, ok := r.out[BundleConfig]; !ok {
		return nil, fmt.Errorf("bundle requires %s", BundleConfig)
	}
	return r.out, nil
}

type bundleReader struct {
	fsys    fs.FS
	out     Bundle
	total   int
	entries int
}

func (r *bundleReader) walk(dir string) error {
	entries, err := fs.ReadDir(r.fsys, dir)
	if err != nil {
		return fmt.Errorf("read bundle directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if r.entries++; r.entries > 2*MaxBundleFiles {
			return errors.New("bundle has too many files")
		}
		full := path.Join(dir, name)
		typ := entry.Type()
		switch {
		case typ&fs.ModeSymlink != 0:
			return errors.New("symbolic links are not allowed in a bundle")
		case typ.IsDir():
			if err := r.walk(full); err != nil {
				return err
			}
		case typ.IsRegular():
			if err := r.file(full); err != nil {
				return err
			}
		default:
			return errors.New("bundle contains an unsupported file type")
		}
	}
	return nil
}

func (r *bundleReader) file(full string) error {
	if len(r.out) >= MaxBundleFiles {
		return errors.New("bundle has too many files")
	}
	f, err := r.fsys.Open(full)
	if err != nil {
		return fmt.Errorf("read bundle file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("read bundle file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("bundle contains an unsupported file type")
	}
	if info.Size() > MaxBundleFileBytes {
		return fmt.Errorf("bundle file exceeds %d bytes", MaxBundleFileBytes)
	}
	if r.total += int(info.Size()); r.total > MaxBundleBytes {
		return fmt.Errorf("bundle exceeds %d bytes", MaxBundleBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBundleFileBytes+1))
	if err != nil {
		return fmt.Errorf("read bundle file: %w", err)
	}
	if len(data) > MaxBundleFileBytes {
		return fmt.Errorf("bundle file exceeds %d bytes", MaxBundleFileBytes)
	}
	r.out[full] = data
	return nil
}

// source is the file tree a compile reads: the OS (diskRoot set) or memory.
// It records every file read so a Set can reproduce its Bundle.
type source struct {
	fsys     fs.FS
	diskRoot string
	reads    map[string][]byte
}

// confine validates relative lexically and (on disk) against symbolic links,
// returning the slash path below base.
func (src *source) confine(base, relative string) (string, error) {
	if filepath.IsAbs(relative) || strings.HasPrefix(relative, "/") {
		return "", fmt.Errorf("absolute path is not allowed")
	}
	slashed := filepath.ToSlash(relative)
	for _, part := range strings.Split(slashed, "/") {
		if part == ".." {
			return "", fmt.Errorf("path parent components are not allowed")
		}
	}
	clean := path.Clean(slashed)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes config directory")
	}
	full := path.Join(base, clean)
	current := base
	for _, part := range strings.Split(clean, "/") {
		current = path.Join(current, part)
		info, err := fs.Lstat(src.fsys, current)
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("lstat %s: no such file or directory", current)
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("symbolic links are not allowed")
		}
	}
	return full, nil
}

func readFS(fsys fs.FS, name string, limit int) ([]byte, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return nil, fmt.Errorf("file is not regular or exceeds %d bytes", limit)
	}
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, err
	}
	return data, nil
}

// read reads a confined file and records it for Bundle.
func (src *source) read(name string, limit int) ([]byte, error) {
	data, err := readFS(src.fsys, name, limit)
	if err != nil {
		return nil, err
	}
	if src.reads == nil {
		src.reads = map[string][]byte{}
	}
	src.reads[name] = data
	return data, nil
}

func (src *source) readConfig(name string) ([]byte, error) {
	return readFS(src.fsys, name, MaxConfigBytes)
}

// bundle assembles the files the compile read plus the config under BundleConfig.
func (src *source) bundle(configName string, config []byte) (Bundle, error) {
	if _, clash := src.reads[BundleConfig]; clash && configName != BundleConfig {
		return nil, fmt.Errorf("a referenced file collides with %s", BundleConfig)
	}
	out := make(Bundle, len(src.reads)+1)
	for name, data := range src.reads {
		out[name] = data
	}
	out[BundleConfig] = bytes.Clone(config)
	return out, nil
}

// snapshot captures a file read from full. Disk files keep their location so
// they can be re-checked; memory files carry only the bundle path.
func (src *source) snapshot(name, full string, data []byte) SkillFile {
	file := SkillFile{Name: name, Digest: fmt.Sprintf("%x", sha256.Sum256(data)), Data: bytes.Clone(data)}
	if src.diskRoot == "" {
		file.Relative = full
		return file
	}
	file.Root = src.diskRoot
	file.Relative = filepath.FromSlash(full)
	file.Path = filepath.Join(src.diskRoot, file.Relative)
	return file
}

// bundleFS is a read-only in-memory fs.FS over a validated Bundle.
type bundleFS Bundle

func (b bundleFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if data, ok := b[name]; ok {
		return &memFile{info: memInfo{name: path.Base(name), size: int64(len(data))}, Reader: *bytes.NewReader(data)}, nil
	}
	if _, err := b.ReadDir(name); err != nil {
		return nil, err
	}
	return &memFile{info: memInfo{name: path.Base(name), dir: true}, fsys: b, dir: name}, nil
}

func (b bundleFS) ReadDir(name string) ([]fs.DirEntry, error) {
	prefix := ""
	if name != "." {
		prefix = name + "/"
	}
	seen := map[string]fs.DirEntry{}
	for key, data := range b {
		rest, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		child, _, nested := strings.Cut(rest, "/")
		if nested {
			seen[child] = memInfo{name: child, dir: true}
		} else {
			seen[child] = memInfo{name: child, size: int64(len(data))}
		}
	}
	if len(seen) == 0 && name != "." {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	out := make([]fs.DirEntry, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

type memFile struct {
	bytes.Reader
	info memInfo
	fsys bundleFS
	dir  string
}

func (f *memFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *memFile) Close() error               { return nil }
func (f *memFile) Read(p []byte) (int, error) {
	if f.info.dir {
		return 0, &fs.PathError{Op: "read", Path: f.dir, Err: errors.New("is a directory")}
	}
	return f.Reader.Read(p)
}
func (f *memFile) ReadDir(int) ([]fs.DirEntry, error) { return f.fsys.ReadDir(f.dir) }

type memInfo struct {
	name string
	size int64
	dir  bool
}

func (i memInfo) Name() string { return i.name }
func (i memInfo) Size() int64  { return i.size }
func (i memInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i memInfo) ModTime() time.Time         { return time.Time{} }
func (i memInfo) IsDir() bool                { return i.dir }
func (i memInfo) Sys() any                   { return nil }
func (i memInfo) Type() fs.FileMode          { return i.Mode().Type() }
func (i memInfo) Info() (fs.FileInfo, error) { return i, nil }
