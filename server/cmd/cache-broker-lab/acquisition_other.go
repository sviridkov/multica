//go:build cachebrokerlab && !darwin && !windows

package main

import "errors"

func newAcquisitionDomain(config, acquisitionSlot, string) (acquisitionDomain, string, error) {
	return nil, "", errors.New("native acquisition experiment requires macOS or Windows")
}
func ordinaryAcquisitionIdentity() (string, error) {
	return "", errors.New("native acquisition experiment requires macOS or Windows")
}
