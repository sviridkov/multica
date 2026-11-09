package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

var ErrCLIConfigConflict = errors.New("CLI config changed or another writer is active; reload the profile and reapply the edit")

type configSnapshot struct {
	path   string
	exists bool
	data   []byte
}

func (c CLIConfig) matchesSnapshot(path string, current []byte, exists bool) bool {
	if c.snapshot == nil {
		return !exists
	}
	s := c.snapshot
	return s.path == path && s.exists == exists && bytes.Equal(s.data, current)
}

// RawMessage avoids rounding numbers in future settings. A separate map at
// each typed object level also preserves future backend and OpenClaw fields.
type configFields map[string]json.RawMessage

func decodeConfigFields(data []byte, value any) (configFields, error) {
	var fields configFields
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("CLI config settings must be a JSON object")
	}
	if err := json.Unmarshal(data, value); err != nil {
		return nil, err
	}
	t := reflect.TypeOf(value).Elem()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		key := strings.Split(f.Tag.Get("json"), ",")[0]
		if key == "" || key == "-" {
			continue
		}
		// encoding/json also recognizes case-insensitive matches of a tag.
		for name := range fields {
			if strings.EqualFold(name, key) {
				delete(fields, name)
			}
		}
	}
	return fields, nil
}

func encodeConfigFields(value any, unknown configFields) ([]byte, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var fields configFields
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, err
	}
	for key, value := range unknown {
		if _, known := fields[key]; !known {
			fields[key] = value
		}
	}
	return json.Marshal(fields)
}

func (c *CLIConfig) UnmarshalJSON(data []byte) error {
	type plain CLIConfig
	var value plain
	unknown, err := decodeConfigFields(data, &value)
	if err != nil {
		return err
	}
	// A present null is not a duration. Missing and zero have different meanings.
	var fields configFields
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for name := range fields {
		if strings.EqualFold(name, "gc_completed_task_ttl") && value.GCCompletedTaskTTL == nil {
			return errors.New("gc_completed_task_ttl must be a duration string, not null")
		}
	}
	*c = CLIConfig(value)
	c.unknown = unknown
	return nil
}

func (c CLIConfig) MarshalJSON() ([]byte, error) {
	type plain CLIConfig
	return encodeConfigFields(plain(c), c.unknown)
}

func (c *BackendOverrides) UnmarshalJSON(data []byte) error {
	type plain BackendOverrides
	var value plain
	unknown, err := decodeConfigFields(data, &value)
	if err != nil {
		return err
	}
	*c = BackendOverrides(value)
	c.unknown = unknown
	return nil
}

func (c BackendOverrides) MarshalJSON() ([]byte, error) {
	type plain BackendOverrides
	return encodeConfigFields(plain(c), c.unknown)
}

func (c *OpenClawOverride) UnmarshalJSON(data []byte) error {
	type plain OpenClawOverride
	var value plain
	unknown, err := decodeConfigFields(data, &value)
	if err != nil {
		return err
	}
	*c = OpenClawOverride(value)
	c.unknown = unknown
	return nil
}

func (c OpenClawOverride) MarshalJSON() ([]byte, error) {
	type plain OpenClawOverride
	return encodeConfigFields(plain(c), c.unknown)
}

// ResetSettings clears settings understood by this version, preserving the
// loaded revision and unknown fields. Setup uses this instead of a blind
// replacement so confirmation cannot erase newer or future-version settings.
func (c *CLIConfig) ResetSettings() {
	next := CLIConfig{unknown: c.unknown, snapshot: c.snapshot}
	if c.Backends != nil {
		backends := &BackendOverrides{unknown: c.Backends.unknown}
		if c.Backends.OpenClaw != nil && len(c.Backends.OpenClaw.unknown) > 0 {
			backends.OpenClaw = &OpenClawOverride{unknown: c.Backends.OpenClaw.unknown}
		}
		if len(backends.unknown) > 0 || backends.OpenClaw != nil {
			next.Backends = backends
		}
	}
	*c = next
}

// Revision identifies the exact loaded bytes, including whether the file existed.
// It is a conflict token, not authorization. A missing snapshot has no revision.
func (c CLIConfig) Revision() string {
	if c.snapshot == nil {
		return ""
	}
	if !c.snapshot.exists {
		return "missing"
	}
	sum := sha256.Sum256(c.snapshot.data)
	return hex.EncodeToString(sum[:])
}
