//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillsInstallCLI(t *testing.T) {
	// No daemon exists; skills are a local client operation.
	state := t.TempDir()
	root := filepath.Join(t.TempDir(), "skills with spaces")
	path := filepath.Join(root, "boxpool-workspace", "SKILL.md")
	for i := 0; i < 2; i++ {
		code, out, errOut := cli(t, state, "", "skills", "install", "--dir", root)
		if code != 0 || errOut != "" || !strings.Contains(out, path) {
			t.Fatalf("install = %d %q %q", code, out, errOut)
		}
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("custom"), 0644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := cli(t, state, "", "skills", "install", "--dir", root)
	if code != 1 || !strings.Contains(errOut, "--force") {
		t.Fatalf("custom content = %d %q", code, errOut)
	}
	code, _, errOut = cli(t, state, "", "skills", "install", "--dir", root, "--force")
	if code != 0 || errOut != "" {
		t.Fatalf("force = %d %q", code, errOut)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(original) {
		t.Fatalf("force did not restore bundled content: %v", err)
	}
	if files, err := os.ReadDir(state); err != nil || len(files) != 0 {
		t.Fatalf("installer touched daemon state: %v, %v", files, err)
	}
}

func TestSkillsHelpAndArgumentsDoNotWrite(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-created")
	state := t.TempDir()
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"skills"}, 2},
		{[]string{"skills", "--help"}, 0},
		{[]string{"skills", "install", "--dir", root, "--help"}, 0},
		{[]string{"skills", "unknown"}, 2},
		{[]string{"skills", "install", "--dir", ""}, 2},
		{[]string{"skills", "install", "--dir", " \t"}, 2},
		{[]string{"skills", "install", "--dir", root, "unexpected"}, 2},
		{[]string{"skills", "install", "--dir", root, "--unknown"}, 2},
	} {
		code, _, _ := cli(t, state, "", tc.args...)
		if code != tc.code {
			t.Errorf("%v = %d, want %d", tc.args, code, tc.code)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("help/invalid arguments wrote the skill: %v", err)
	}
}
