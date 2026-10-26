//go:build cachebrokerlab && windows

package daemon

import (
	"encoding/json"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
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
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(endpoint.PID))
	if err == windows.ERROR_INVALID_PARAMETER {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	status, err := windows.WaitForSingleObject(h, 10000)
	if err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatal("service process has not released its kill-on-close job", err, status)
	}
}

func nativePrivate(t *testing.T, path string) {
	t.Helper()
	out, err := exec.Command("icacls.exe", path, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F").CombinedOutput()
	if err != nil {
		t.Fatalf("private credential ACL: %v %s", err, out)
	}
}
func nativeService(t *testing.T, cfg nativeLab, control, name string) (func(), func()) {
	t.Helper()
	m, err := mgr.Connect()
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.CreateService(name, cfg.Service, mgr.Config{StartType: mgr.StartManual, DisplayName: "Disposable Multica cache broker", ServiceStartName: "LocalSystem"}, "--config", filepath.Join(control, "config.json"), "--name", name)
	if err != nil {
		m.Disconnect()
		t.Fatal(err)
	}
	stop := func() {
		st, err := s.Query()
		if err != nil {
			t.Error(err)
			return
		}
		if st.State == svc.Stopped {
			return
		}
		if _, err := s.Control(svc.Stop); err != nil {
			t.Error(err)
			return
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			st, err = s.Query()
			if err != nil {
				t.Error(err)
				return
			}
			if st.State == svc.Stopped {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Error("SCM service failed to stop")
	}
	start := func() {
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		stop()
		if err := s.Delete(); err != nil {
			t.Error(err)
		}
		s.Close()
		m.Disconnect()
	})
	return start, stop
}
