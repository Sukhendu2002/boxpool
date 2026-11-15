package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstall(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom skills")
	path, err := Install(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, Name, "SKILL.md"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	assertContent(t, path, workspaceSkill)
	// An identical install is a no-op, including when force is given.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		if _, err := Install(root, force); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil || !os.SameFile(info, after) || !info.ModTime().Equal(after.ModTime()) {
			t.Fatalf("identical install rewrote the skill: %v", err)
		}
	}
}

func TestInstallPreservesCustomization(t *testing.T) {
	root := t.TempDir()
	path, err := Install(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("custom instructions"), 0644); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(filepath.Dir(path), "notes.txt")
	if err := os.WriteFile(sibling, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(root, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("different install = %v, want overwrite guidance", err)
	}
	assertContent(t, path, "custom instructions")
	if _, err := Install(root, true); err != nil {
		t.Fatal(err)
	}
	assertContent(t, path, workspaceSkill)
	assertContent(t, sibling, "keep")
	if files, err := os.ReadDir(filepath.Dir(path)); err != nil || len(files) != 2 {
		t.Fatalf("temporary files left after install: %v, %v", files, err)
	}
}

func TestInstallRejectsInvalidDestinations(t *testing.T) {
	for _, root := range []string{"", " \t"} {
		if _, err := Install(root, false); err == nil {
			t.Errorf("Install(%q) succeeded", root)
		}
	}
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{file, filepath.Join(file, "skills")} {
		if _, err := Install(target, true); err == nil {
			t.Errorf("Install(%q) succeeded", target)
		}
	}
	if err := os.Mkdir(filepath.Join(root, Name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, Name, "SKILL.md"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(root, true); err == nil {
		t.Fatal("installed over a directory")
	}
	assertContent(t, file, "keep")
}

func TestInstallDoesNotFollowSkillLinks(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "directory"}[directory], func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			dir := filepath.Join(root, Name)
			path := filepath.Join(outside, "SKILL.md")
			if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			if directory {
				if err := os.Symlink(outside, dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path, filepath.Join(dir, "SKILL.md")); err != nil {
					t.Fatal(err)
				}
			}
			for _, force := range []bool{false, true} {
				if _, err := Install(root, force); err == nil {
					t.Fatal("installed through a symlink")
				}
			}
			assertContent(t, path, "keep")
		})
	}
}

func TestInstallForceDoesNotTruncateHardLink(t *testing.T) {
	root := t.TempDir()
	path, err := Install(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("custom"), 0644); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if err := os.Link(path, backup); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(root, true); err != nil {
		t.Fatal(err)
	}
	assertContent(t, path, workspaceSkill)
	assertContent(t, backup, "custom")
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("unexpected contents in %s", path)
	}
}
