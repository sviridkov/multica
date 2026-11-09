package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
)

func seedProfileBytes(t *testing.T, document string) string {
	t.Helper()
	path, err := cli.CLIConfigPath()
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

func TestSetupPreservesUnknownSettingsAndResetsKnownSettings(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	path := seedProfileBytes(t, `{"token":"synthetic-secret","poll_interval":"10s","future":[null,9007199254740993],"backends":{"openclaw":{"state_dir":"old","future":true},"new_backend":{"x":1}}}`)
	cfg, err := cli.LoadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	ok, err := persistSelfHostConfigIfReachable(cfg, "https://new.invalid", "https://app.invalid", "", func(string) bool { return true })
	if err != nil || !ok {
		t.Fatalf("setup save failed: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("synthetic-secret")) || bytes.Contains(b, []byte("poll_interval")) || bytes.Contains(b, []byte("state_dir")) {
		t.Fatal("known settings were not reset")
	}
	for _, key := range []string{"future", "9007199254740993", "new_backend"} {
		if !strings.Contains(string(b), key) {
			t.Errorf("setup dropped %s", key)
		}
	}
}

func TestSetupRejectsChangedProfileDuringProbe(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	path := seedProfileBytes(t, `{"server_url":"https://old.invalid"}`)
	newer := []byte(`{"device_name":"newer","future":null}`)
	cfg, err := cli.LoadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	ok, err := persistSelfHostConfigIfReachable(cfg, "https://new.invalid", "https://app.invalid", "", func(string) bool {
		if err := os.WriteFile(path, newer, 0600); err != nil {
			t.Fatal(err)
		}
		return true
	})
	if err == nil || ok {
		t.Fatal("setup silently replaced a newer profile")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, newer) {
		t.Fatal("setup changed newer bytes")
	}
}

func TestSetupRejectsMalformedProfile(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	path := seedProfileBytes(t, `{"token":`)
	if _, _, err := confirmOverwrite("", "https://new.invalid", ""); err == nil {
		t.Fatal("malformed profile treated as fresh setup")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"token":` {
		t.Fatal("malformed bytes changed")
	}
}

func TestConfigPatchRequiresExactRevisionAndPreservesUnknowns(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	path := seedProfileBytes(t, `{"token":"old-fixture","future":900719925474099312345}`)
	cfg, err := cli.LoadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	revision := cfg.Revision()
	cmd := newConfigTestCmd()
	body, _ := json.Marshal(map[string]any{"expected_revision": revision, "changes": map[string]any{"token": nil, "server_url": "https://new.invalid"}})
	cmd.SetIn(bytes.NewReader(body))
	if err := runConfigPatch(cmd, nil); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(path)
	if !bytes.Contains(current, []byte("900719925474099312345")) || bytes.Contains(current, []byte("token")) {
		t.Fatal("patch lost future settings or restored unset token")
	}
	cmd.SetIn(bytes.NewReader(body))
	if !errors.Is(runConfigPatch(cmd, nil), cli.ErrCLIConfigConflict) {
		t.Fatal("stale patch accepted")
	}
	again, _ := os.ReadFile(path)
	if !bytes.Equal(current, again) {
		t.Fatal("stale patch changed file")
	}
}

func TestConfigSetConditionalRollbackRejectsNewerRevision(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	path := seedProfileBytes(t, `{"gc_completed_task_ttl":"72h"}`)
	cfg, err := cli.LoadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	cmd := newConfigTestCmd()
	cmd.Flags().String("expect-revision", cfg.Revision(), "")
	if err := runConfigSet(cmd, []string{"gc_completed_task_ttl", "336h"}); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(path)
	if !errors.Is(runConfigSet(cmd, []string{"gc_completed_task_ttl", "72h"}), cli.ErrCLIConfigConflict) {
		t.Fatal("rollback clobbered a newer revision")
	}
	again, _ := os.ReadFile(path)
	if !bytes.Equal(current, again) {
		t.Fatal("rollback changed newer bytes")
	}
}

func TestConfigPatchInvalidInputDoesNotPublish(t *testing.T) {
	for _, body := range []string{`{}`, `{"expected_revision":"missing","changes":{"device_name":"x"}}`, `{"expected_revision":"missing","changes":{"token":42}}`, `{"expected_revision":"missing","changes":{}} {}`, `{"expected_revision":"missing","changes":{},"extra":true}`} {
		t.Run(body, func(t *testing.T) {
			redirectTestHome(t, t.TempDir())
			cmd := newConfigTestCmd()
			cmd.SetIn(strings.NewReader(body))
			if err := runConfigPatch(cmd, nil); err == nil {
				t.Fatal("invalid patch accepted")
			}
			path, _ := cli.CLIConfigPath()
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("invalid patch published")
			}
		})
	}
}
