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
	if err := writeTar(out, src); err != nil {
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

func writeTar(w io.Writer, src string) error {
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
	if err := extractTar(r.Body, dest); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func extractTar(r io.Reader, dest string) error {
	// The tar comes from the host; the guest is not a boundary against itself, and safe host extraction is #16's job.
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
		if err := extractEntry(tr, dest, h); err != nil {
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
		p, err := entryPath(dest, name)
		if err == nil {
			err = setMetadata(p, dirs[name])
		}
		if err != nil {
			return fmt.Errorf("entry %q: %w", name, err)
		}
	}
	return nil
}

func entryPath(dest, name string) (string, error) {
	clean := path.Clean(name)
	if name == "" || path.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") || !filepath.IsLocal(filepath.FromSlash(clean)) {
		return "", errors.New("path escapes destination")
	}
	p := dest
	parts := strings.Split(clean, "/")
	for _, part := range parts[:len(parts)-1] {
		p = filepath.Join(p, part)
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(p, 0755); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", errors.New("parent is not a directory (symlink parents are not allowed)")
		}
	}
	return filepath.Join(dest, filepath.FromSlash(clean)), nil
}

func extractEntry(tr io.Reader, dest string, h *tar.Header) error {
	if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeSymlink {
		return fmt.Errorf("unsupported tar type %d", h.Typeflag)
	}
	p, err := entryPath(dest, h.Name)
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
			return os.Mkdir(p, 0700)
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
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
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
