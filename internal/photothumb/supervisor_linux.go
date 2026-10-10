//go:build linux

package photothumb

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func verifyWorkerPath(path string) error {
	if path != WorkerPath || !safePath(path) {
		return errors.New("photo worker path is not the accepted absolute path")
	}
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	current := string(filepath.Separator)
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("stat photo worker path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("photo worker path contains a symbolic link")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return errors.New("photo worker path is not root-owned")
		}
		if info.Mode().Perm()&0o022 != 0 {
			return errors.New("photo worker path is writable by group or others")
		}
		if index == len(parts)-1 && info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			return errors.New("photo worker must not have set-user-id or set-group-id permissions")
		}
		if index != len(parts)-1 && !info.IsDir() {
			return errors.New("photo worker path parent is not a directory")
		}
		if index == len(parts)-1 && (!info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0) {
			return errors.New("photo worker is not an executable regular file")
		}
	}
	if err := syscall.Access(path, 1); err != nil {
		return fmt.Errorf("photo worker is not executable by the server: %w", err)
	}
	return nil
}

func configureProcess(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	return nil
}

func setCloseOnExecForExtraFiles() error {
	directory, err := os.Open("/proc/self/fd")
	if err != nil {
		return fmt.Errorf("open process descriptor directory: %w", err)
	}
	syscall.ForkLock.Lock()
	entries, readErr := directory.ReadDir(-1)
	if readErr == nil {
		for _, entry := range entries {
			descriptor, parseErr := strconv.Atoi(entry.Name())
			if parseErr != nil || descriptor < 3 {
				continue
			}
			flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(descriptor), syscall.F_GETFD, 0)
			if errno == syscall.EBADF {
				continue
			}
			if errno != 0 {
				readErr = fmt.Errorf("read descriptor %d flags: %w", descriptor, errno)
				break
			}
			if flags&syscall.FD_CLOEXEC != 0 {
				continue
			}
			if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(descriptor), syscall.F_SETFD, flags|syscall.FD_CLOEXEC); errno != 0 {
				readErr = fmt.Errorf("mark descriptor %d close-on-exec: %w", descriptor, errno)
				break
			}
		}
	}
	syscall.ForkLock.Unlock()
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	return nil
}

func killProcessGroup(pid int) error {
	if pid <= 0 {
		return errors.New("invalid photo worker process id")
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
