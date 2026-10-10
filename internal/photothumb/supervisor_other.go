//go:build !linux

package photothumb

import (
	"errors"
	"os/exec"
)

func verifyWorkerPath(string) error {
	return errors.New("photo worker supervision requires Linux")
}

func configureProcess(*exec.Cmd) error {
	return errors.New("photo worker supervision requires Linux")
}

func setCloseOnExecForExtraFiles() error {
	return errors.New("photo worker supervision requires Linux")
}

func killProcessGroup(int) error {
	return errors.New("photo worker supervision requires Linux")
}
