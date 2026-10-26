//go:build cachebrokerlab && (darwin || linux)

package main

import (
	"context"
	"os/signal"
	"syscall"
)

func nativeRun(_ string, run func(context.Context) error) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	return run(ctx)
}
