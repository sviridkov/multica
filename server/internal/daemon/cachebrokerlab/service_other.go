//go:build !darwin && !linux && !windows

package cachebrokerlab

import (
	"errors"
	"os"
)

func lockService(string) (*os.File, error)    { return nil, errors.New("unsupported service platform") }
func replaceServiceFile(string, string) error { return errors.New("unsupported service platform") }
func privateCredential(string) error          { return errors.New("unsupported service platform") }
