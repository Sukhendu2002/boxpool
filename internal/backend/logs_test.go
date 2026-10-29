package backend

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConsoleLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	if _, err := OpenConsole(context.Background(), path, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing log: %v", err)
	}
	if err := os.WriteFile(path, []byte("initial output\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenConsole(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("later\n")
	f.Close()
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "initial output\n" {
		t.Fatalf("snapshot = %q, %v", b, err)
	}
}

func TestFollowConsole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "console.log")
	os.WriteFile(path, []byte("initial output\n"), 0600)
	r, err := OpenConsole(ctx, path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	read := func(want string) {
		t.Helper()
		b := make([]byte, len(want))
		if _, err := io.ReadFull(r, b); err != nil || string(b) != want {
			t.Fatalf("read = %q, %v; want %q", b, err, want)
		}
	}
	read("initial output\n")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString("appended\n")
	f.Close()
	read("appended\n")
	// Copy-truncate to a shorter file, as the VM log rotators do.
	os.WriteFile(path, []byte("new\n"), 0600)
	read("new\n")
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("replaced\n"), 0600)
	read("replaced\n")
	cancel()
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled follow = %v", err)
	}
}
