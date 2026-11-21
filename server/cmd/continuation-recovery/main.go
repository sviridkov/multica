// continuation-recovery is a separate operator tool. It is never invoked by
// the detector or a task-token endpoint, and cannot submit external operations.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/continuation"
)

func read(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 32769))
	if err != nil {
		return err
	}
	if len(data) > 32768 {
		return errors.New("input exceeds 32KiB")
	}
	// Disallow unknown fields so raw stderr/payload/credentials cannot silently
	// become accepted operator evidence.
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}

func run() error {
	operation := flag.String("operation", "reconcile", "reconcile (read only externally) or recover-prestart")
	input := flag.String("input", "", "bounded JSON request file")
	snapshot := flag.String("snapshot", "", "verified deployment/SMS observation file; no raw API payload")
	flag.Parse()
	if *input == "" || os.Getenv("DATABASE_URL") == "" {
		return errors.New("input and DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	m, err := continuation.FromEnv(pool, nil)
	if err != nil {
		return err
	}
	if m == nil {
		return errors.New("pilot project flag required")
	}
	var result any
	switch *operation {
	case "recover-prestart":
		if *snapshot != "" {
			return errors.New("snapshot is only for reconciliation")
		}
		var req continuation.RecoveryRequest
		if err = read(*input, &req); err != nil {
			return err
		}
		result, err = m.RecoverPreStart(ctx, req)
	case "reconcile":
		var req continuation.OperationRequest
		if err = read(*input, &req); err != nil {
			return err
		}
		var observer continuation.Observer = continuation.HTTPObserver{GitHubToken: os.Getenv("GITHUB_TOKEN"), RetellToken: os.Getenv("RETELL_API_KEY")}
		if *snapshot != "" {
			if req.Operation.Kind != "deploy" && req.Operation.Kind != "sms" {
				return errors.New("merge/call require authoritative provider lookup")
			}
			var s continuation.Observation
			if err = read(*snapshot, &s); err != nil {
				return err
			}
			observer = continuation.SnapshotObserver{Snapshot: s}
		}
		result, err = m.ReconcileOperation(ctx, req, observer)
	default:
		return errors.New("unsupported operation")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
