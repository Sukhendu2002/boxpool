package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The Engine API versions boxpool speaks. 1.41 is Docker 20.10; 1.47 is
// Docker 27, the newest whose API this driver was checked against. Newer
// engines still serve it: Docker 29 accepts 1.44 and up.
const (
	minAPI = "1.41"
	maxAPI = "1.47"
)

// Timeouts; tests may shorten them.
var (
	apiTimeout     = 30 * time.Second // one Engine API request
	pullTimeout    = 10 * time.Minute // pulling the image
	archiveTimeout = 10 * time.Minute // copying a sandbox's files to or from a snapshot
)

// DefaultHost is the Engine API socket used when DOCKER_HOST is unset.
const DefaultHost = "/var/run/docker.sock"

// host returns the Engine API socket path: DOCKER_HOST if set, which must be
// a unix:// URL, else DefaultHost. Docker contexts are not read.
func host() (string, error) {
	h := os.Getenv("DOCKER_HOST")
	if h == "" {
		return DefaultHost, nil
	}
	if path, ok := strings.CutPrefix(h, "unix://"); ok && path != "" {
		return path, nil
	}
	return "", fmt.Errorf("DOCKER_HOST=%s: boxpool only talks to Docker over a local unix:// socket", h)
}

// engine is a minimal Docker Engine API client over a unix socket.
type engine struct {
	c      *http.Client
	base   string // URL prefix with the negotiated version, e.g. http://docker/v1.47
	socket string // for error messages
	Info   engineVersion
}

type engineVersion struct {
	Version       string
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string
	Os            string
	Arch          string
}

// dialEngine connects to the Engine API at socket and negotiates the API
// version: the newest both sides speak.
func dialEngine(ctx context.Context, socket string) (*engine, error) {
	c := &http.Client{Transport: &http.Transport{
		// No Proxy: the request never leaves this host.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		MaxIdleConns: 4,
	}}
	return negotiate(ctx, c, "http://docker", socket)
}

func negotiate(ctx context.Context, c *http.Client, base, socket string) (*engine, error) {
	e := &engine{c: c, base: base, socket: socket}
	if err := e.do(ctx, http.MethodGet, "/version", nil, nil, &e.Info); err != nil {
		return nil, err
	}
	v := e.Info.APIVersion
	if apiLess(maxAPI, v) {
		v = maxAPI
	}
	if apiLess(v, minAPI) {
		return nil, fmt.Errorf("Docker %s speaks Engine API %s; boxpool needs %s (Docker 20.10) or newer", e.Info.Version, e.Info.APIVersion, minAPI)
	}
	if e.Info.MinAPIVersion != "" && apiLess(v, e.Info.MinAPIVersion) {
		return nil, fmt.Errorf("Docker %s needs Engine API %s or newer; boxpool speaks %s to %s", e.Info.Version, e.Info.MinAPIVersion, minAPI, maxAPI)
	}
	e.base += "/v" + v
	return e, nil
}

// apiLess reports whether API version a is older than b. Versions are
// "major.minor"; anything unparsable counts as 0.0.
func apiLess(a, b string) bool {
	parse := func(s string) (int, int) {
		ma, mi, _ := strings.Cut(s, ".")
		x, _ := strconv.Atoi(ma)
		y, _ := strconv.Atoi(mi)
		return x, y
	}
	am, an := parse(a)
	bm, bn := parse(b)
	return am < bm || am == bm && an < bn
}

// apiError is an Engine API response with a status of 300 or more.
type apiError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("docker: %s %s: %d %s: %s", e.Method, e.Path, e.Status, http.StatusText(e.Status), e.Message)
}

// statusIs reports whether err is an Engine API error with that status.
func statusIs(err error, status int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == status
}

// do sends one request, encoding body as JSON unless it is nil, and decodes
// a 2xx response into out unless it is nil. It gives up after apiTimeout.
func (e *engine) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	resp, err := e.send(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return err
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("docker: %s %s: decode response: %w", method, path, err)
	}
	return nil
}

// send sends one request and returns a 2xx response for the caller to read,
// or an error that explains a failure to reach the engine. A body that is
// an io.Reader is sent as a tar, anything else as JSON.
func (e *engine) send(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var r io.Reader
	contentType := "application/json"
	switch b := body.(type) {
	case nil:
	case io.Reader:
		r, contentType = b, "application/x-tar"
	default:
		j, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(j)
	}
	u := e.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := e.c.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrPermission):
			return nil, fmt.Errorf("docker: permission denied on %s: run as root or as a member of the docker group (which is root-equivalent): %w", e.socket, err)
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
			return nil, fmt.Errorf("docker: no Docker Engine is listening on %s (is dockerd running?): %w", e.socket, err)
		}
		return nil, fmt.Errorf("docker: %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var msg struct{ Message string }
	if json.Unmarshal(b, &msg) != nil || msg.Message == "" {
		msg.Message = string(bytes.TrimSpace(b))
	}
	return nil, &apiError{Method: method, Path: path, Status: resp.StatusCode, Message: msg.Message}
}

// pull pulls repo at tag, a tag or a digest, and returns once the engine
// has it. Errors arrive in the progress stream of a 200 response.
func (e *engine) pull(ctx context.Context, repo, tag string) error {
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()
	// An empty tag would pull every tag of the repository.
	if tag == "" {
		return fmt.Errorf("docker: pull %s: no tag or digest", repo)
	}
	resp, err := e.send(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {repo}, "tag": {tag}}, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var m struct {
			Error       string
			ErrorDetail struct{ Message string }
		}
		if err := dec.Decode(&m); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("docker: pull %s: %w", repo, err)
		}
		if m.ErrorDetail.Message != "" {
			m.Error = m.ErrorDetail.Message
		}
		if m.Error != "" {
			return fmt.Errorf("docker: pull %s %s: %s", repo, tag, m.Error)
		}
	}
}
