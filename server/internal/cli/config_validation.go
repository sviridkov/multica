package cli

import (
	"fmt"
	"time"
)

// Keep file writers subject to the same bounds as config set. OpenClaw's
// cli_timeout intentionally retains its documented clamp/ignore semantics.
func validateConfigSettings(c CLIConfig) error {
	if c.MaxConcurrentTasks < 0 {
		return fmt.Errorf("max_concurrent_tasks must be non-negative")
	}
	for _, field := range []struct {
		name, value string
		zero        bool
	}{
		{"poll_interval", c.PollInterval, false},
		{"ws_claim_poll_interval", c.WSClaimPollInterval, false},
		{"heartbeat_interval", c.HeartbeatInterval, false},
		{"codex_semantic_inactivity_timeout", c.CodexSemanticInactivityTimeout, false},
		{"codex_handshake_timeout", c.CodexHandshakeTimeout, false},
		{"auto_update_check_interval", c.AutoUpdateCheckInterval, false},
		{"agent_idle_watchdog", c.AgentIdleWatchdog, true},
		{"agent_tool_watchdog", c.AgentToolWatchdog, true},
	} {
		if field.value == "" {
			continue
		}
		d, err := time.ParseDuration(field.value)
		if err != nil || d < 0 || (!field.zero && d == 0) {
			return fmt.Errorf("invalid %s duration", field.name)
		}
	}
	if c.AgentTimeout != nil {
		d, err := time.ParseDuration(*c.AgentTimeout)
		if err != nil || d < 0 {
			return fmt.Errorf("invalid agent_timeout duration")
		}
	}
	if c.GCCompletedTaskTTL != nil {
		d, err := ParseDurationWithDays(*c.GCCompletedTaskTTL)
		if err != nil || d < 0 {
			return fmt.Errorf("invalid gc_completed_task_ttl duration")
		}
	}
	return nil
}
