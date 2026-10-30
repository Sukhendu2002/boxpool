package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestExposes(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	ctx := context.Background()
	web, err := s.CreateExpose(ctx, Expose{Port: 41000, Sandbox: "web", SandboxPort: 8000, TokenID: "abc", URL: "https://h.ts.net:41000/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateExpose(ctx, Expose{Port: 41001, Sandbox: "web", SandboxPort: 8000, URL: "x"}); err == nil {
		t.Error("exposing a port twice succeeded")
	}
	if _, err := s.CreateExpose(ctx, Expose{Port: 41002, Sandbox: "api", SandboxPort: 3000, Public: true, URL: "y"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetExpose(ctx, "web", 8000); err != nil || got != web {
		t.Errorf("GetExpose = %+v, %v; want %+v", got, err, web)
	}
	if list, err := s.ListExposes(ctx); err != nil || len(list) != 2 || list[0] != web || !list[1].Public {
		t.Errorf("ListExposes = %+v, %v", list, err)
	}
	if err := s.DeleteExpose(ctx, web.Port); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetExpose(ctx, "web", 8000); !errors.Is(err, ErrExposeNotFound) {
		t.Errorf("GetExpose after delete = %v, want ErrExposeNotFound", err)
	}
	if err := s.DeleteExpose(ctx, web.Port); !errors.Is(err, ErrExposeNotFound) {
		t.Errorf("DeleteExpose twice = %v, want ErrExposeNotFound", err)
	}
}
