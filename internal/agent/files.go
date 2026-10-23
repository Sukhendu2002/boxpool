package agent

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

func getFiles(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("path")
	if src == "" {
		writeError(w, http.StatusBadRequest, errors.New("path is required"))
		return
	}
	src = filepath.Clean(src)
	if _, err := os.Lstat(src); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	// Reject unsupported entries before the response status is committed.
	if err := filepath.Walk(src, func(name string, info fs.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("entry %q: %w", name, err)
		}
		return checkFileType(name, info)
	}); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	out := &tarResponse{ResponseWriter: w}
	if err := WriteTar(out, src); err != nil {
		if out.started {
			// A JSON error inside a tar would look like a successful download.
			panic(http.ErrAbortHandler)
		}
		writeError(w, http.StatusBadRequest, err)
	}
}

type tarResponse struct {
	http.ResponseWriter
	started bool
}

func (w *tarResponse) Write(b []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(b)
}

func checkFileType(name string, info fs.FileInfo) error {
	if !info.Mode().IsRegular() && !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("entry %q: unsupported file type %s", name, info.Mode())
	}
	return nil
}

// WriteTar writes src, a file or a directory tree, as a tar rooted at its
// base name, the format of GET /v1/files: modes and modification times are
// kept, symlinks stay symlinks, owners are dropped. Other file types are
// refused.
func WriteTar(w io.Writer, src string) error {
	src = filepath.Clean(src)
	base := filepath.Base(src)
	if base == ".." {
		// Parent paths need a local archive root; Abs cleans without following symlinks.
		abs, err := filepath.Abs(src)
		if err != nil {
			return err
		}
		base = filepath.Base(abs)
	}
	if base == string(filepath.Separator) {
		base = "."
	}
	tw := tar.NewWriter(w)
	err := filepath.Walk(src, func(name string, info fs.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("entry %q: %w", name, err)
		}
		if err := checkFileType(name, info); err != nil {
			return err
		}
		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(name)
			if err != nil {
				return fmt.Errorf("entry %q: %w", name, err)
			}
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, name)
		if err != nil {
			return err
		}
		h.Name = path.Join(filepath.ToSlash(base), filepath.ToSlash(rel))
		h.Format = tar.FormatPAX // keep subsecond mtimes
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(name)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			closeErr := f.Close()
			if err != nil {
				return err
			}
			return closeErr
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

func putFiles(w http.ResponseWriter, r *http.Request) {
	dest := r.URL.Query().Get("path")
	if dest == "" {
		writeError(w, http.StatusBadRequest, errors.New("path is required"))
		return
	}
	// Resolve DEST once; all archive paths below it reject symlink parents.
	dest, err := filepath.EvalSymlinks(dest)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	info, err := os.Stat(dest)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, errors.New("destination is not a directory"))
		return
	}
	if err := ExtractTar(r.Body, dest, nil); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ExtractTar extracts a tar into the existing directory dest, as PUT
// /v1/files does. If check is not nil, it sees each header first and may
// change it or refuse the entry. Only regular files, directories and
// symlinks are extracted, with their modes and modification times. No entry
// is written outside dest: absolute names and .. are refused, parents must
// be real directories, never symlinks, and an existing entry is unlinked,
// not written through. Symlinks themselves may point anywhere.
func ExtractTar(r io.Reader, dest string, check func(*tar.Header) error) error {
	// Files and directories are created through root, so the kernel keeps
	// them in dest even if the checks below missed a way out.
	// shortcut: symlinks, modes and times are set by path, because Go 1.24's
	// os.Root cannot; move them onto root once go.mod reaches 1.25.
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(r)
	dirs := make(map[string]*tar.Header)
	last := "<header>"
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("entry after %q: %w", last, err)
		}
		last = h.Name
		if check != nil {
			if err := check(h); err != nil {
				return fmt.Errorf("entry %q: %w", last, err)
			}
		}
		if err := extractEntry(tr, root, dest, h); err != nil {
			return fmt.Errorf("entry %q: %w", h.Name, err)
		}
		if h.Typeflag == tar.TypeDir {
			dirs[path.Clean(h.Name)] = h
		}
	}
	// Children change parent mtimes; apply directory metadata deepest first.
	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == "." || names[j] == "." {
			return names[j] == "." && names[i] != "."
		}
		return len(names[i]) > len(names[j])
	})
	for _, name := range names {
		_, p, err := entryPath(dest, name)
		if err == nil {
			err = setMetadata(p, dirs[name])
		}
		if err != nil {
			return fmt.Errorf("entry %q: %w", name, err)
		}
	}
	return nil
}

// entryPath returns the archive name's path relative to dest and joined
// to it, after creating missing parents and checking that every parent is
// a directory.
func entryPath(dest, name string) (rel, full string, err error) {
	clean := path.Clean(name)
	if name == "" || path.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") || !filepath.IsLocal(filepath.FromSlash(clean)) {
		return "", "", errors.New("path escapes destination")
	}
	p := dest
	parts := strings.Split(clean, "/")
	for _, part := range parts[:len(parts)-1] {
		p = filepath.Join(p, part)
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(p, 0755); err != nil {
				return "", "", err
			}
			continue
		}
		if err != nil {
			return "", "", err
		}
		if !info.IsDir() {
			return "", "", errors.New("parent is not a directory (symlink parents are not allowed)")
		}
	}
	rel = filepath.FromSlash(clean)
	return rel, filepath.Join(dest, rel), nil
}

func extractEntry(tr io.Reader, root *os.Root, dest string, h *tar.Header) error {
	if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeSymlink {
		return fmt.Errorf("unsupported tar type %d", h.Typeflag)
	}
	rel, p, err := entryPath(dest, h.Name)
	if err != nil {
		return err
	}
	info, err := os.Lstat(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if h.Typeflag == tar.TypeDir {
		if err == nil && !info.IsDir() {
			return errors.New("directory conflicts with an existing entry")
		}
		if err != nil {
			return root.Mkdir(rel, 0700)
		}
		return os.Chmod(p, info.Mode()|0700) // allow contents before restoring the archived mode
	}
	if err == nil {
		if info.IsDir() {
			return errors.New("entry conflicts with an existing directory")
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return errors.New("entry conflicts with an unsupported file type")
		}
		// Unlink first: neither symlinks nor existing hard links may redirect writes.
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	if h.Typeflag == tar.TypeSymlink {
		if err := os.Symlink(h.Linkname, p); err != nil {
			return err
		}
		return setSymlinkMetadata(p, h.FileInfo().Mode(), h.ModTime)
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, tr)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return setMetadata(p, h)
}

func setMetadata(p string, h *tar.Header) error {
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if (h.Typeflag == tar.TypeDir && !info.IsDir()) || (h.Typeflag == tar.TypeReg && !info.Mode().IsRegular()) {
		return errors.New("entry changed type before metadata was restored")
	}
	if err := os.Chmod(p, h.FileInfo().Mode()); err != nil {
		return err
	}
	return os.Chtimes(p, h.ModTime, h.ModTime)
}
