package main

import (
	"encoding/json"
	"io"
	"os"
	"strconv"
	"syscall"
	"time"
)

type report struct {
	Args              []string
	Environment       []string
	WorkingDir        string
	Descriptors       []int
	DescriptorTargets []string
	PID               int
	ProcessGroup      int
}

func main() {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		os.Exit(2)
	}
	processGroup, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		os.Exit(2)
	}
	descriptors, descriptorTargets := openDescriptors()
	state := report{
		Args:              os.Args[1:],
		Environment:       os.Environ(),
		WorkingDir:        workingDir,
		Descriptors:       descriptors,
		DescriptorTargets: descriptorTargets,
		PID:               os.Getpid(),
		ProcessGroup:      processGroup,
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(string(input), encoded, 0o600); err != nil {
		os.Exit(2)
	}
	time.Sleep(10 * time.Second)
}

func openDescriptors() ([]int, []string) {
	var descriptors []int
	var targets []string
	for descriptor := uintptr(3); descriptor < 64; descriptor++ {
		if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, descriptor, syscall.F_GETFD, 0); errno == 0 {
			descriptors = append(descriptors, int(descriptor))
			target, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(descriptor)))
			if err != nil {
				os.Exit(2)
			}
			targets = append(targets, target)
		}
	}
	return descriptors, targets
}
