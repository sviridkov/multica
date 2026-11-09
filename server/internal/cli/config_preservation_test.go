package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedConfigDocument(t *testing.T, profile, document string) string {
	t.Helper()
	path, err := CLIConfigPathForProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readConfigBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCLIConfig_UnknownValuesAndUnset(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	path := seedConfigDocument(t, "", `{"gc_completed_task_ttl":"72h","agent_idle_watchdog":"5m","future":{"integer":9007199254740993123456789,"decimal":1.234567890123456789,"array":[null,{"x":true}],"null":null},"backends":{"openclaw":{"state_dir":"/old","future":[1,null,{"n":999999999999999999999999}]},"other":{"precise":1.0000000000000000001}}}`)
	cfg, err := LoadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.GCCompletedTaskTTL = nil
	cfg.AgentIdleWatchdog = "0s"
	cfg.Backends.OpenClaw.StateDir = ""
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(readConfigBytes(t, path), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["gc_completed_task_ttl"]; ok {
		t.Fatal("unset restored TTL override")
	}
	for key, want := range map[string]string{
		"future":   `{"integer":9007199254740993123456789,"decimal":1.234567890123456789,"array":[null,{"x":true}],"null":null}`,
		"backends": `{"openclaw":{"future":[1,null,{"n":999999999999999999999999}]},"other":{"precise":1.0000000000000000001}}`,
	} {
		// Decode with UseNumber, rather than float64, so rounded numbers fail.
		canonical := func(b []byte) string {
			d := json.NewDecoder(bytes.NewReader(b))
			d.UseNumber()
			var v any
			if err := d.Decode(&v); err != nil {
				t.Fatal(err)
			}
			out, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			return string(out)
		}
		if len(got[key]) == 0 || canonical(got[key]) != canonical([]byte(want)) {
			t.Errorf("%s lost an unknown value", key)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatal("config is not private")
	}
}

func TestCLIConfig_StaleSnapshotCannotOverwriteNewDocument(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	path := seedConfigDocument(t, "", `{"device_name":"old"}`)
	stale, err := LoadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := LoadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	fresh.DeviceName = "new"
	if err := SaveCLIConfig(fresh); err != nil {
		t.Fatal(err)
	}
	// An external writer before the save also must invalidate the snapshot.
	seedConfigDocument(t, "", `{"device_name":"new","future":{"n":9007199254740993}}`)
	before := readConfigBytes(t, path)
	stale.RuntimeName = "stale update"
	if err := SaveCLIConfig(stale); err == nil {
		t.Fatal("stale snapshot overwrote newer document")
	}
	if !bytes.Equal(before, readConfigBytes(t, path)) {
		t.Fatal("conflict changed original bytes")
	}
	if err := SaveCLIConfig(CLIConfig{DeviceName: "blind replacement"}); err == nil {
		t.Fatal("unloaded replacement overwrote existing profile")
	}
}

func TestCLIConfig_ProfileSnapshotCannotBeMoved(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	seedConfigDocument(t, "one", `{"device_name":"one"}`)
	cfg, err := LoadCLIConfigForProfile("one")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveCLIConfigForProfile(cfg, "two"); err == nil {
		t.Fatal("snapshot from another profile was accepted")
	}
	path, _ := CLIConfigPathForProfile("two")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("other profile was published")
	}
}

func TestCLIConfig_InvalidDocumentCannotBeReplaced(t *testing.T) {
	for _, document := range []string{`{"token":`, `[]`, `null`, `{"max_concurrent_tasks":"bad"}`, `{"gc_completed_task_ttl":null}`, `{"backends":{"openclaw":42}}`} {
		t.Run(document, func(t *testing.T) {
			redirectTestHome(t, t.TempDir())
			path := seedConfigDocument(t, "", document)
			if _, err := LoadCLIConfig(); err == nil {
				t.Fatal("invalid document was accepted")
			}
			if err := SaveCLIConfig(CLIConfig{DeviceName: "replacement"}); err == nil {
				t.Fatal("invalid document was overwritten")
			}
			if !bytes.Equal([]byte(document), readConfigBytes(t, path)) {
				t.Fatal("original bytes changed")
			}
		})
	}
}

func TestCLIConfig_ConcurrentWriterProcesses(t *testing.T) {
	for _, scenario := range []string{"new", "existing", "separate"} {
		separate := scenario == "separate"
		name := scenario + " profile"
		if separate {
			name = "separate profiles"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			redirectTestHome(t, home)
			if scenario == "existing" {
				seedConfigDocument(t, "same", `{"future":9007199254740993}`)
			}
			barrier := t.TempDir()
			var children []*exec.Cmd
			for _, id := range []string{"a", "b"} {
				profile := "same"
				if separate {
					profile = id
				}
				cmd := exec.Command(os.Args[0], "-test.run=^TestCLIConfig_WriterProcess$")
				cmd.Env = append(os.Environ(), "CONFIG_WRITER_HOME="+home, "CONFIG_WRITER_BARRIER="+barrier, "CONFIG_WRITER_ID="+id, "CONFIG_WRITER_PROFILE="+profile)
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				children = append(children, cmd)
			}
			defer func() {
				for _, cmd := range children {
					if cmd.ProcessState == nil {
						_ = cmd.Process.Kill()
						_ = cmd.Wait()
					}
				}
			}()
			deadline := time.Now().Add(10 * time.Second)
			for _, id := range []string{"a", "b"} {
				for {
					if _, err := os.Stat(filepath.Join(barrier, id+".ready")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("writer did not reach barrier")
					}
					time.Sleep(time.Millisecond)
				}
			}
			if err := os.WriteFile(filepath.Join(barrier, "go"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			for _, cmd := range children {
				if err := cmd.Wait(); err != nil {
					t.Fatalf("writer failed: %v", err)
				}
			}
			results := string(readConfigBytes(t, filepath.Join(barrier, "a.result"))) + string(readConfigBytes(t, filepath.Join(barrier, "b.result")))
			wantSuccess := 1
			if separate {
				wantSuccess = 2
			}
			if strings.Count(results, "saved") != wantSuccess || strings.Count(results, "conflict") != 2-wantSuccess {
				t.Fatalf("expected %d successful writers with other snapshots rejected; outcomes %s", wantSuccess, results)
			}
		})
	}
}

func TestCLIConfig_WriterProcess(t *testing.T) {
	home := os.Getenv("CONFIG_WRITER_HOME")
	if home == "" {
		return
	}
	redirectTestHome(t, home)
	profile, id, barrier := os.Getenv("CONFIG_WRITER_PROFILE"), os.Getenv("CONFIG_WRITER_ID"), os.Getenv("CONFIG_WRITER_BARRIER")
	cfg, err := LoadCLIConfigForProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DeviceName = id
	if err := os.WriteFile(filepath.Join(barrier, id+".ready"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(barrier, "go")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("barrier timeout")
		}
		time.Sleep(time.Millisecond)
	}
	result := "saved"
	if err := SaveCLIConfigForProfile(cfg, profile); err != nil {
		if !errors.Is(err, ErrCLIConfigConflict) {
			t.Fatal(err)
		}
		result = "conflict"
	}
	if err := os.WriteFile(filepath.Join(barrier, id+".result"), []byte(result), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCLIConfig_InvalidKnownSettingDoesNotPublish(t *testing.T) {
	for _, edit := range []struct {
		name  string
		apply func(*CLIConfig)
	}{
		{"negative concurrency", func(c *CLIConfig) { c.MaxConcurrentTasks = -1 }},
		{"invalid interval", func(c *CLIConfig) { c.PollInterval = "not a duration" }},
		{"zero positive interval", func(c *CLIConfig) { c.HeartbeatInterval = "0s" }},
		{"negative watchdog", func(c *CLIConfig) { c.AgentToolWatchdog = "-1s" }},
		{"invalid TTL", func(c *CLIConfig) { v := "bad"; c.GCCompletedTaskTTL = &v }},
	} {
		t.Run(edit.name, func(t *testing.T) {
			redirectTestHome(t, t.TempDir())
			path := seedConfigDocument(t, "", `{"future":null}`)
			cfg, err := LoadCLIConfig()
			if err != nil {
				t.Fatal(err)
			}
			edit.apply(&cfg)
			if err := SaveCLIConfig(cfg); err == nil {
				t.Fatal("invalid known value was published")
			}
			if string(readConfigBytes(t, path)) != `{"future":null}` {
				t.Fatal("original bytes changed")
			}
		})
	}
}
