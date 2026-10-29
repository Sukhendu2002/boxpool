package backend

import (
	"context"
	"errors"
	"io"
	"os"
	"time"
)

// OpenConsole reads the current console log, optionally waiting for new
// bytes. Follow reopens renamed logs and rewinds copy-truncated logs.
func OpenConsole(ctx context.Context, path string, follow bool) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	return &consoleReader{f: f, ctx: ctx, cancel: cancel, path: path, follow: follow, end: fi.Size()}, nil
}

type consoleReader struct {
	f           *os.File
	ctx         context.Context
	cancel      context.CancelFunc
	path        string
	follow      bool
	offset, end int64
}

func (r *consoleReader) Close() error {
	r.cancel()
	return r.f.Close()
}

func (r *consoleReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		if !r.follow {
			if r.offset >= r.end {
				return 0, io.EOF
			}
			p = p[:min(int64(len(p)), r.end-r.offset)]
		}
		n, err := r.f.Read(p)
		r.offset += int64(n)
		if n > 0 || err != io.EOF || !r.follow {
			return n, err
		}
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		fi, err := os.Stat(r.path)
		if errors.Is(err, os.ErrNotExist) {
			continue // a rename may briefly leave no current file
		}
		if err != nil {
			return 0, err
		}
		old, err := r.f.Stat()
		if err != nil {
			return 0, err
		}
		if !os.SameFile(fi, old) {
			f, err := os.Open(r.path)
			if err != nil {
				return 0, err
			}
			r.f.Close()
			r.f, r.offset = f, 0
		} else if fi.Size() < r.offset {
			// shortcut: copy-truncate can drop bytes; use a broker for lossless logs.
			if _, err := r.f.Seek(0, io.SeekStart); err != nil {
				return 0, err
			}
			r.offset = 0
		}
	}
}
