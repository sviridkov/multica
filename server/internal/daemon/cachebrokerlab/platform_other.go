//go:build !darwin && !linux && !windows

package cachebrokerlab

import (
	"fmt"
	"os"
)

func protectedPath(string) error { return fmt.Errorf("unsupported platform") }
func entryMetadata(string, os.FileInfo) (string, error) {
	return "", fmt.Errorf("unsupported platform")
}
func replaceJournal(string) error                 { return fmt.Errorf("unsupported platform") }
func producerEnvironment(Policy, string) []string { return nil }

func publishReadable(string) error { return fmt.Errorf("unsupported platform") }
