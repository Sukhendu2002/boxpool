package docker

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

func (d *Driver) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	c, err := d.inspect(ctx, id) // checks the owner label before reading logs
	if err != nil {
		return nil, err
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "follow": {strconv.FormatBool(follow)}}
	resp, err := d.e.send(ctx, http.MethodGet, "/containers/"+c.ID+"/logs", q, nil)
	if err != nil {
		return nil, err
	}
	// Boxpool creates non-TTY containers; Docker frames stdout and stderr.
	return &logReader{ReadCloser: resp.Body}, nil
}

type logReader struct {
	io.ReadCloser
	remaining uint32
}

func (r *logReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.remaining == 0 {
		var header [8]byte
		if _, err := io.ReadFull(r.ReadCloser, header[:]); err != nil {
			return 0, err
		}
		if (header[0] != 1 && header[0] != 2) || header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return 0, fmt.Errorf("docker: invalid log frame")
		}
		r.remaining = binary.BigEndian.Uint32(header[4:])
	}
	n, err := r.ReadCloser.Read(p[:min(uint64(len(p)), uint64(r.remaining))])
	r.remaining -= uint32(n)
	if err == io.EOF && r.remaining != 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
