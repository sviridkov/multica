package main

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

// Desktop uses stdin so credentials never appear in argv, logs, or output.
// Only its two owned fields cross the JS boundary; unknown JSON stays in Go.
var configPatchCmd = &cobra.Command{
	Use: "patch-auth", Hidden: true, Args: exactArgs(0),
	RunE: runConfigPatch,
}

func init() { configCmd.AddCommand(configPatchCmd) }

func runConfigPatch(cmd *cobra.Command, _ []string) error {
	if err := requireTaskLocalConfigRoot(); err != nil {
		return err
	}
	var patch struct {
		ExpectedRevision string                     `json:"expected_revision"`
		Changes          map[string]json.RawMessage `json:"changes"`
	}
	decoder := json.NewDecoder(io.LimitReader(cmd.InOrStdin(), 1024*1024+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil {
		return errors.New("invalid profile patch")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || patch.ExpectedRevision == "" || patch.Changes == nil {
		return errors.New("invalid profile patch")
	}
	profile := resolveProfile(cmd)
	cfg, err := cli.LoadCLIConfigForProfile(profile)
	if err != nil {
		return err
	}
	if cfg.Revision() != patch.ExpectedRevision {
		return cli.ErrCLIConfigConflict
	}
	for key, raw := range patch.Changes {
		var value string
		if string(raw) != "null" {
			if json.Unmarshal(raw, &value) != nil {
				return errors.New("profile patch values must be strings or null")
			}
		}
		switch key {
		case "server_url":
			cfg.ServerURL = value
		case "token":
			cfg.Token = value
		default:
			return errors.New("profile patch only supports server_url and token")
		}
	}
	return cli.SaveCLIConfigForProfile(cfg, profile)
}
