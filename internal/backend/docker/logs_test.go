package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func logFrame(stream byte, text string) []byte {
	b := make([]byte, 8+len(text))
	b[0] = stream
	binary.BigEndian.PutUint32(b[4:8], uint32(len(text)))
	copy(b[8:], text)
	return b
}

func TestLogReader(t *testing.T) {
	frames := append(logFrame(1, ""), logFrame(1, "out\n")...)
	frames = append(frames, logFrame(2, "err\n")...)
	for _, tc := range []struct {
		name string
		data []byte
		want string
		err  bool
	}{
		{"streams", frames, "out\nerr\n", false},
		{"short header", []byte{1}, "", true},
		{"short payload", logFrame(1, "abc")[:10], "ab", true},
		{"invalid stream", logFrame(3, "oops"), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &logReader{ReadCloser: io.NopCloser(bytes.NewReader(tc.data))}
			b, err := io.ReadAll(r)
			if string(b) != tc.want || (err != nil) != tc.err {
				t.Errorf("logs = %q, %v", b, err)
			}
		})
	}
}

func TestDriverLogs(t *testing.T) {
	d := &Driver{root: "/owned", prefix: "bp-"}
	foreign := false
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/json") {
			c := container{ID: "container-id", Name: "/bp-box"}
			c.Config.Labels = map[string]string{labelOwner: d.root, labelSandbox: "box"}
			if foreign {
				c.Config.Labels[labelOwner] = "/other"
			}
			json.NewEncoder(w).Encode(c)
			return
		}
		calls++
		if r.URL.Path != "/containers/container-id/logs" || r.URL.Query().Get("follow") != "true" || r.URL.Query().Get("stderr") != "1" || r.URL.Query().Get("stdout") != "1" {
			t.Errorf("unexpected log request: %s", r.URL)
		}
		w.Write(logFrame(1, "console\n"))
	}))
	defer srv.Close()
	d.e = &engine{c: srv.Client(), base: srv.URL}
	r, err := d.Logs(context.Background(), "box", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(b) != "console\n" {
		t.Fatalf("logs = %q, %v", b, err)
	}
	foreign = true
	if _, err := d.Logs(context.Background(), "box", true); !errors.Is(err, errForeign) || calls != 1 {
		t.Errorf("foreign logs = %v, log calls %d", err, calls)
	}
}
