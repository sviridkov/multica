//go:build cachebrokerlab && darwin

package daemon

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func nativeWaitServiceTree(t *testing.T, control string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(control, "endpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	var endpoint struct{ PID int }
	if err := json.Unmarshal(data, &endpoint); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("/bin/ps", "-axo", "pgid=").Output()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, pgid := range strings.Fields(string(out)) {
			if pgid == strconv.Itoa(endpoint.PID) {
				found = true
				break
			}
		}
		if !found {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("launchd service process group survived crash; reconciliation forbidden")
}

func nativePrivate(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
}
func nativeService(t *testing.T, cfg nativeLab, control, name string) (func(), func()) {
	t.Helper()
	escape := func(v string) string { var b strings.Builder; xml.EscapeText(&b, []byte(v)); return b.String() }
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array><string>%s</string><string>--config</string><string>%s</string><string>--name</string><string>%s</string></array><key>RunAtLoad</key><true/><key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string></dict></plist>`, escape(name), escape(cfg.Service), escape(filepath.Join(control, "config.json")), escape(name), escape(filepath.Join(control, "service.log")), escape(filepath.Join(control, "service.log")))
	path := filepath.Join(control, "service.plist")
	if err := os.WriteFile(path, []byte(plist), 0600); err != nil {
		t.Fatal(err)
	}
	loaded := false
	start := func() {
		out, err := exec.Command("/bin/launchctl", "bootstrap", "system", path).CombinedOutput()
		if err != nil {
			t.Fatalf("launchd bootstrap: %v %s", err, out)
		}
		loaded = true
	}
	stop := func() {
		if !loaded {
			return
		}
		out, err := exec.Command("/bin/launchctl", "bootout", "system/"+name).CombinedOutput()
		if err != nil {
			t.Errorf("launchd bootout: %v %s", err, out)
			return
		}
		loaded = false
		// bootout removes the registration before all process resources have
		// necessarily drained. Volume cleanup is registered before this stop.
		nativeWaitServiceTree(t, control)
	}
	t.Cleanup(stop)
	return start, stop
}
