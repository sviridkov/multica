package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

func writeGCProfile(t *testing.T, profile, data string) {
	t.Helper()
	path, err := cli.CLIConfigPathForProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigGCProfile(t *testing.T) {
	stageFakeAgent(t)
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TASK_CONFIG_ROOT", "")
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("MULTICA_GC_COMPLETED_TASK_TTL", "")
	writeGCProfile(t, "retained", `{"gc_completed_task_ttl":"72h"}`)
	overrides := Overrides{Profile: "retained", ServerURL: "https://api.multica.ai", WorkspacesRoot: t.TempDir()}
	cfg, err := LoadConfig(overrides)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GCCompletedTaskTTL != 72*time.Hour {
		t.Fatalf("TTL=%s, want profile 72h", cfg.GCCompletedTaskTTL)
	}
	if cfg.GCTTL != DefaultGCTTL || cfg.GCOrphanTTL != DefaultGCOrphanTTL {
		t.Fatal("other TTL defaults changed")
	}
	overrides.Profile = "other"
	cfg, err = LoadConfig(overrides)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GCCompletedTaskTTL != 336*time.Hour {
		t.Fatalf("profile leaked: %s", cfg.GCCompletedTaskTTL)
	}
	overrides.Profile = "retained"
	t.Setenv("MULTICA_GC_COMPLETED_TASK_TTL", "0")
	cfg, err = LoadConfig(overrides)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GCCompletedTaskTTL != 0 {
		t.Fatal("env must win")
	}
}

func TestLoadConfigGCProfileInvalid(t *testing.T) {
	stageFakeAgent(t)
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TASK_CONFIG_ROOT", "")
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("MULTICA_GC_COMPLETED_TASK_TTL", "")
	for _, body := range []string{`{"gc_completed_task_ttl":"garbage"}`, `{"gc_completed_task_ttl":"-1h"}`, `{"gc_completed_task_ttl":"999999999999999999h"}`, `{"gc_completed_task_ttl":72}`, `{"gc_completed_task_ttl":""}`, `{"gc_completed_task_ttl":null}`, `{broken`, `null`} {
		t.Run(body, func(t *testing.T) {
			writeGCProfile(t, "invalid", body)
			if _, err := LoadConfig(Overrides{Profile: "invalid", AllowNoAgents: true}); err == nil {
				t.Fatal("invalid profile silently accepted")
			}
		})
	}
}

func TestLoadConfigGCProfileNewProcess(t *testing.T) {
	if os.Getenv("MULTICA_TEST_GC_PROFILE_CHILD") == "1" {
		redirectTestHome(t, os.Getenv("MULTICA_TEST_GC_PROFILE_HOME"))
		cfg, err := LoadConfig(Overrides{Profile: "persisted", ServerURL: "https://api.multica.ai", AllowNoAgents: true})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GCCompletedTaskTTL != 72*time.Hour {
			t.Fatalf("new process TTL=%s, want 72h", cfg.GCCompletedTaskTTL)
		}
		return
	}
	stageFakeAgent(t)
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TASK_CONFIG_ROOT", "")
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing"))
	writeGCProfile(t, "persisted", `{"gc_completed_task_ttl":"72h"}`)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestLoadConfigGCProfileNewProcess$")
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "MULTICA_GC_COMPLETED_TASK_TTL=") && !strings.HasPrefix(item, "MULTICA_TEST_GC_PROFILE_CHILD=") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	testHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(cmd.Env, "MULTICA_TEST_GC_PROFILE_CHILD=1", "MULTICA_TEST_GC_PROFILE_HOME="+testHome)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh process: %v\n%s", err, out)
	}
}

func TestLoadConfigGCProfileDisableAndInvalidEnv(t *testing.T) {
	stageFakeAgent(t)
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TASK_CONFIG_ROOT", "")
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("MULTICA_GC_COMPLETED_TASK_TTL", "")
	writeGCProfile(t, "disabled", `{"gc_completed_task_ttl":"0"}`)
	overrides := Overrides{Profile: "disabled", ServerURL: "https://api.multica.ai", WorkspacesRoot: t.TempDir()}
	cfg, err := LoadConfig(overrides)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GCCompletedTaskTTL != 0 {
		t.Fatalf("profile disable ignored: %s", cfg.GCCompletedTaskTTL)
	}
	for _, value := range []string{"broken", "-1h", "999999999999999999999h"} {
		t.Setenv("MULTICA_GC_COMPLETED_TASK_TTL", value)
		if _, err := LoadConfig(overrides); err == nil || !strings.Contains(err.Error(), "MULTICA_GC_COMPLETED_TASK_TTL") {
			t.Fatalf("invalid env %q fell back to profile: %v", value, err)
		}
	}
}
