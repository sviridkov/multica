//go:build darwin || linux

package cachebrokerlab

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

func protectedPath(path string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("isolated privileged broker identity required; euid=%d", os.Geteuid())
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("relative protected path")
	}
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		st, err := os.Lstat(current)
		if err != nil {
			return err
		}
		s, ok := st.Sys().(*syscall.Stat_t)
		if !ok || s.Uid != 0 || st.Mode().Perm()&0022 != 0 || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unprotected component %s", current)
		}
		if err := noACL(current); err != nil {
			return err
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return nil
}
func noExtraMetadata(path string) error {
	n, err := unix.Listxattr(path, nil)
	if err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("extended attributes require a reviewed policy: %s", path)
	}
	return noACL(path)
}
func noACL(path string) error {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("/bin/ls", "-lde", path).Output()
		if err != nil {
			return err
		}
		fields := strings.Fields(string(out))
		if len(fields) == 0 || strings.Contains(fields[0], "+") {
			return fmt.Errorf("ACL requires a reviewed policy: %s", path)
		}
	}
	return nil
}
func entryMetadata(path string, info os.FileInfo) (string, error) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("missing native metadata")
	}
	if s.Uid != 0 || info.Mode().Perm()&0022 != 0 || (info.Mode().IsRegular() && s.Nlink != 1) {
		return "", fmt.Errorf("unexpected owner/mode/hardlink: %s", path)
	}
	if err := noExtraMetadata(path); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d", s.Dev, s.Ino, s.Uid, s.Gid, s.Nlink, info.Mode(), info.ModTime().UnixNano()), nil
}
func replaceJournal(root string) error {
	if err := os.Rename(filepath.Join(root, "journal.new"), filepath.Join(root, "journal.json")); err != nil {
		return err
	}
	f, err := os.Open(root)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func producerEnvironment(p Policy, kind string) []string {
	return []string{"PATH=" + filepath.Dir(p.Git) + ":/usr/bin:/bin", "TMPDIR=" + filepath.Join(p.Root, "tmp"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(p.Root, "empty.gitconfig"), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=3", "GIT_CONFIG_KEY_0=url.file://" + filepath.ToSlash(p.Source) + ".insteadOf", "GIT_CONFIG_VALUE_0=https://cache-lab.invalid/fixture.git", "GIT_CONFIG_KEY_1=protocol.file.allow", "GIT_CONFIG_VALUE_1=always", "GIT_CONFIG_KEY_2=core.hooksPath", "GIT_CONFIG_VALUE_2=" + filepath.Join(p.Root, "no-hooks"), "MULTICA_CACHE_LAB_ALLOW=1", "MULTICA_CACHE_LAB_HOME=" + filepath.Join(p.Root, "codex-home"), "MULTICA_CACHE_LAB_KIND=" + kind, "MULTICA_CACHE_LAB_OID=" + p.SourceOID}
}

func publishReadable(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, 0755)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported producer entry: %s", path)
		}
		return os.Chmod(path, (info.Mode().Perm()|0444)&^0022)
	})
}
