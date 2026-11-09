//go:build linux

// An offline, dependency-free synthetic writer/observer. Never a production launcher.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func event(kind string, fields map[string]any) {
	fields["event"] = kind
	fields["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(fields)
	fmt.Println(string(b))
}
func fatal(err error) {
	event("error", map[string]any{"error": err.Error()})
	os.Exit(1)
}
func spawn(args []string, session bool) *exec.Cmd {
	cmd := exec.Command("/probe", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if session {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := cmd.Start(); err != nil {
		fatal(err)
	}
	return cmd
}
func writer(name string, delay time.Duration) {
	// Stay alive through graceful stop. The cgroup must kill these descendants.
	signal.Ignore(syscall.SIGTERM)
	time.Sleep(delay)
	sid, _, _ := syscall.Syscall(syscall.SYS_GETSID, 0, 0, 0)
	if err := os.WriteFile(filepath.Join("/data", name), []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0600); err != nil {
		fatal(err)
	}
	event("writer", map[string]any{"name": name, "pid": os.Getpid(), "ppid": os.Getppid(), "sid": sid})
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if err := os.WriteFile(filepath.Join("/data", name), []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0600); err != nil {
			fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func observe(target string) {
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(target) {
		fatal(fmt.Errorf("invalid target ID"))
	}
	self, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		fatal(err)
	}
	var relative string
	for _, line := range strings.Split(string(self), "\n") {
		if strings.HasPrefix(line, "0::") {
			relative = strings.TrimPrefix(line, "0::")
		}
	}
	// cgroupfs Docker puts siblings under the same parent. Do not search or
	// inspect unrelated groups. Refuse unrecognized layouts rather than guess.
	base := filepath.Base(relative)
	match := regexp.MustCompile(`^(?:docker-)?([a-f0-9]{64})(?:\.scope)?$`).FindStringSubmatch(base)
	if match == nil {
		fatal(fmt.Errorf("unrecognized observer cgroup layout: %s", relative))
	}
	name := strings.Replace(base, match[1], target, 1)
	path := filepath.Join("/sys/fs/cgroup", filepath.Dir(relative), name, "cgroup.events")
	file, err := os.Open(path)
	if err != nil {
		fatal(err)
	}
	defer file.Close()
	seenPopulated := false
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := file.Seek(0, 0); err != nil {
			fatal(err)
		}
		data, err := io.ReadAll(file)
		if err != nil {
			fatal(err)
		}
		value := string(data)
		if strings.Contains(value, "populated 1\n") && !seenPopulated {
			seenPopulated = true
			event("kernel_populated", map[string]any{"path": path, "target": target, "raw": value})
		}
		if strings.Contains(value, "populated 0\n") && seenPopulated {
			event("kernel_empty", map[string]any{"path": path, "target": target, "raw": value})
			return
		}
		time.Sleep(time.Millisecond)
	}
	fatal(fmt.Errorf("kernel zero not observed before deadline"))
}
func main() {
	if len(os.Args) < 2 {
		fatal(fmt.Errorf("mode required"))
	}
	switch os.Args[1] {
	case "task":
		stopping := make(chan os.Signal, 1)
		signal.Notify(stopping, syscall.SIGTERM)
		go func() {
			<-stopping
			if err := os.WriteFile("/data/stopping", []byte("stop requested"), 0600); err != nil {
				fatal(err)
			}
			event("stop_signal", map[string]any{"pid": os.Getpid()})
		}()
		spawn([]string{"writer", "direct", "0"}, false)
		spawn([]string{"writer", "setsid", "0"}, true)
		spawn([]string{"fork1"}, true)
		spawn([]string{"writer", "late", "1800"}, false)
		spawn([]string{"late-writer"}, false)
		event("task_ready", map[string]any{"pid": os.Getpid()})
		time.Sleep(90 * time.Second)
	case "fork1":
		spawn([]string{"fork2"}, false)
	case "fork2":
		spawn([]string{"writer", "double-fork", "150"}, false)
	case "late-writer":
		signal.Ignore(syscall.SIGTERM)
		deadline := time.Now().Add(60 * time.Second)
		for {
			if _, err := os.Stat("/data/stopping"); err == nil {
				break
			}
			if time.Now().After(deadline) {
				fatal(fmt.Errorf("stop signal not observed"))
			}
			time.Sleep(time.Millisecond)
		}
		writer("late-stop", 0)
	case "writer":
		if len(os.Args) != 4 || !regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(os.Args[2]) {
			fatal(fmt.Errorf("invalid writer args"))
		}
		delay, err := strconv.Atoi(os.Args[3])
		if err != nil || delay < 0 || delay > 5000 {
			fatal(fmt.Errorf("invalid delay"))
		}
		writer(os.Args[2], time.Duration(delay)*time.Millisecond)
	case "observe":
		if len(os.Args) != 3 {
			fatal(fmt.Errorf("target required"))
		}
		observe(os.Args[2])
	default:
		fatal(fmt.Errorf("unknown mode"))
	}
}
