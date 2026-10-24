package backend

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRotateConsole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	if err := RotateConsole(path); err != nil {
		t.Errorf("missing log: %v", err)
	}
	small := bytes.Repeat([]byte("a"), ConsoleMax)
	os.WriteFile(path, small, 0600)
	if err := RotateConsole(path); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, small) {
		t.Errorf("log at the limit changed to %d bytes", len(b))
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("log at the limit rotated: %v", err)
	}

	os.WriteFile(path+".1", []byte("older"), 0600)
	big := append(small, 'b')
	// Appending like firecracker, through an fd held across the rotation.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Write(big)
	if err := RotateConsole(path); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path + ".1"); !bytes.Equal(b, big) {
		t.Errorf("rotated copy has %d bytes, want %d", len(b), len(big))
	}
	f.Write([]byte("after"))
	if b, _ := os.ReadFile(path); string(b) != "after" {
		t.Errorf("log after rotation: %d bytes %.20q", len(b), b)
	}
}
