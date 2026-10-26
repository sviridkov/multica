//go:build cachebrokerlab && darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type darwinAcquisition struct {
	id              acquisitionIdentity
	census, profile string
}

func newAcquisitionDomain(cfg config, slot acquisitionSlot, name string) (acquisitionDomain, string, error) {
	if slot.Identity.UID == 0 || slot.Identity.GID == 0 || cfg.Census == "" {
		return nil, "", errors.New("missing ordinary acquisition identity or census")
	}
	profile := fmt.Sprintf(`(version 1)
(deny default)
(allow file-read*)
(allow sysctl-read)
(allow process-fork)
(allow process-exec)
(allow signal (target same-sandbox))
(allow file-write* (subpath %q))
(allow file-write-data (literal "/dev/null"))
(allow network-inbound (local ip "localhost:*"))
(allow network-outbound (remote ip "localhost:*"))
`, slot.Stage)
	path := filepath.Join(filepath.Dir(cfg.Control), name+".sb")
	if err := immutableProfile(path, []byte(profile)); err != nil {
		return nil, "", err
	}
	d := &darwinAcquisition{id: slot.Identity, census: cfg.Census, profile: path}
	return d, fmt.Sprintf("ruid:%d", d.id.UID), nil
}

func (d *darwinAcquisition) Start(cmd *exec.Cmd) error {
	n, err := d.Active()
	if err != nil {
		return err
	}
	if n != 0 {
		return errors.New("acquisition identity already has live processes")
	}
	if err := acquisitionCommand(cmd); err != nil {
		return err
	}
	exe, args := cmd.Path, append([]string(nil), cmd.Args[1:]...)
	cmd.Path = "/usr/bin/sandbox-exec"
	cmd.Args = append([]string{cmd.Path, "-f", d.profile, exe}, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: d.id.UID, Gid: d.id.GID, Groups: []uint32{d.id.GID}}}
	return cmd.Start()
}
func (d *darwinAcquisition) Active() (uint32, error) {
	data, err := exec.Command(d.census, strconv.FormatUint(uint64(d.id.UID), 10)).Output()
	if err != nil {
		return 0, fmt.Errorf("native RUID census unavailable: %w", err)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32)
	return uint32(n), err
}
func (*darwinAcquisition) Close() error { return nil }
func ordinaryAcquisitionIdentity() (string, error) {
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		return "", errors.New("acquisition launcher must be ordinary")
	}
	return fmt.Sprintf("ruid:%d", os.Getuid()), nil
}
