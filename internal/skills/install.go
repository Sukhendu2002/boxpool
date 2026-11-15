// Package skills ships instructions for host agents using Boxpool workspaces.
package skills

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const Name = "boxpool-workspace"

//go:embed boxpool-workspace/SKILL.md
var workspaceSkill string

// Install writes the bundled skill under a harness's skills directory. It does
// not read harness settings, connect to a daemon or copy credentials. Existing
// instructions must match the bundle unless force explicitly permits replacing
// them. Other files in the skill directory are left alone.
func Install(root string, force bool) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("skills directory must not be empty")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, Name)
	if err := os.MkdirAll(root, 0755); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("skill directory %s must be a real directory, not a symlink or file", dir)
	}
	path := filepath.Join(dir, "SKILL.md")
	info, err = os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s must be a regular file, not a symlink or directory", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if string(data) == workspaceSkill {
			return path, nil
		}
		if !force {
			return "", fmt.Errorf("%s already has different content; use --force to replace it or --dir to choose another skills directory", path)
		}
		// Replace the file atomically, including when the old file has other
		// hard links; never truncate another link's contents.
		f, err := os.CreateTemp(dir, ".SKILL-*")
		if err != nil {
			return "", err
		}
		defer os.Remove(f.Name())
		if err := f.Chmod(0644); err != nil {
			f.Close()
			return "", err
		}
		if err := writeSkill(f); err != nil {
			return "", err
		}
		if err := os.Rename(f.Name(), path); err != nil {
			return "", err
		}
		return path, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// O_EXCL also preserves a file created by a concurrent installer.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return "", err
	}
	if err := writeSkill(f); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

func writeSkill(f *os.File) error {
	_, err := io.WriteString(f, workspaceSkill)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
