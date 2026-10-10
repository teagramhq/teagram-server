//go:build !linux

package main

import "errors"

func setWorkerLimits() error {
	return errors.New("photothumb requires Linux resource limits")
}
