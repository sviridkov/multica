//go:build !windows

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCLIConfig_PublishFailurePreservesOriginal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can bypass the fixture directory permissions")
	}
	redirectTestHome(t, t.TempDir())
	path := seedConfigDocument(t, "", `{"device_name":"before","future":null}`)
	cfg, err := LoadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	// Allow lock acquisition but refuse the temporary publication file.
	f, err := lockCLIConfig(filepath.Join(filepath.Dir(path), ".config.lock"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Chmod(filepath.Dir(path), 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Dir(path), 0700)
	before := readConfigBytes(t, path)
	cfg.DeviceName = "after"
	if err := SaveCLIConfig(cfg); err == nil {
		t.Fatal("publication unexpectedly succeeded")
	}
	if !bytes.Equal(before, readConfigBytes(t, path)) {
		t.Fatal("failed publication changed original bytes")
	}
}
