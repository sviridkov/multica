//go:build cachebrokerlab && !windows

package daemon

import (
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
)

func nativePermissionAttack(path string) error {
	return unix.Setxattr(path, "user.cache-lab-attacker", []byte("attack"), 0)
}

func nativeExtraMetadata(path string) error {
	return unix.Setxattr(path, "user.cache-lab", []byte("preserve this attribute"), 0)
}

func nativeDirectoryLink(link, target string) error { return os.Symlink(target, link) }

func nativeWidenPermissions(path string) error { return os.Chmod(path, 0666) }

func nativePrivileged() bool  { return os.Geteuid() == 0 }
func nativeIdentity() string  { return strconv.Itoa(os.Geteuid()) }
func nativeBaseEnv() []string { return []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"} }
func nativeAttackCommand(t *testing.T, cfg nativeLab, root, role string) *exec.Cmd {
	t.Helper()
	if cfg.UID == 0 {
		t.Fatal("task UID must be unprivileged")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestBrokerAttackChild$", "-test.v")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: cfg.UID, Gid: cfg.GID, Groups: []uint32{cfg.GID}}}
	cmd.Dir = root
	cmd.Env = append(nativeBaseEnv(), "MULTICA_BROKER_ATTACK_ROOT="+root, "MULTICA_BROKER_PRODUCER="+cfg.Producer, "MULTICA_BROKER_ATTACK_ROLE="+role)
	return cmd
}
