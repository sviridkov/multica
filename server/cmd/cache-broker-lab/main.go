//go:build cachebrokerlab

// This command is built only by the disposable native experiment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/multica-ai/multica/server/internal/daemon/cachebrokerlab"
)

type config struct {
	Service      cachebrokerlab.ServiceConfig
	Control      string
	Census       string
	Acquisitions []acquisitionSlot
}

func main() {
	path := flag.String("config", "", "protected bootstrap file")
	name := flag.String("name", "", "native service name")
	acquire := flag.String("acquire", "", "measured producer in an ordinary native domain")
	flag.Parse()
	if *acquire != "" {
		if err := runAcquisition(*acquire); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := nativeRun(*name, func(ctx context.Context) error { return run(ctx, *path) }); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var cfg config
	d := json.NewDecoder(file)
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return err
	}
	if _, err := cachebrokerlab.ServiceTLS(cfg.Control, true); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		log, err := os.OpenFile(filepath.Join(cfg.Control, "service.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		// SCM does not provide a console. Keep this process-lifetime handle open
		// so main can also record a startup failure before the OS closes it.
		os.Stdout, os.Stderr = log, log
	}
	// Fault input is a protected bootstrap file, absent in normal operation.
	// No IPC request can arm it; this binary is excluded from production builds.
	var armed string
	count := 0
	cfg.Service.Fault = func(point string) error {
		data, err := os.ReadFile(filepath.Join(cfg.Control, "fault.json"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var spec struct {
			ID, Point, Mode string
			After           int
		}
		if err := json.Unmarshal(data, &spec); err != nil {
			return err
		}
		if spec.ID != armed {
			armed = spec.ID
			count = 0
		}
		if point != spec.Point {
			return nil
		}
		count++
		if count < spec.After {
			return nil
		}
		if err := os.Remove(filepath.Join(cfg.Control, "fault.json")); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "FAULT", point, spec.Mode)
		if spec.Mode == "exit" {
			os.Exit(86)
		}
		return fmt.Errorf("injected IO failure at %s", point)
	}
	cfg.Service.Production = acquisitionFactory(cfg)
	s, err := cachebrokerlab.OpenService(cfg.Service)
	if err != nil {
		return err
	}
	defer s.Close()
	return cachebrokerlab.ServeService(ctx, s, cfg.Control)
}
