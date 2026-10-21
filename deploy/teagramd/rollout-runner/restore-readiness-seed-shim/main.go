package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	seedPath   = "/run/app/restore-readiness-seed"
	socketDir  = "/run/postgres"
	socketPath = "/run/postgres/.s.PGSQL.5432"
	database   = "127.0.0.1:5432"
	maxClients = 16
)

func main() {
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	if os.Geteuid() != 0 || len(os.Environ()) != 0 || filepath.Dir(socketPath) != socketDir {
		return errors.New("invalid seed-shim runtime")
	}
	directory, err := os.Lstat(socketDir)
	if err != nil || !directory.IsDir() || directory.Mode()&os.ModeSymlink != 0 || directory.Mode().Perm() != 0o700 {
		return errors.New("invalid seed socket directory")
	}
	stat, ok := directory.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("invalid seed socket owner")
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		return errors.New("seed socket already exists")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return errors.New("seed socket unavailable")
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(socketPath, 0o600); err != nil {
		closeErr := listener.Close()
		if closeErr != nil {
			return errors.New("seed socket unavailable")
		}
		return errors.New("seed socket unavailable")
	}
	socketInfo, err := os.Lstat(socketPath)
	if err != nil || socketInfo.Mode()&os.ModeSocket == 0 {
		_ = listener.Close()
		return errors.New("seed socket unavailable")
	}
	socketStat, ok := socketInfo.Sys().(*syscall.Stat_t)
	if !ok || socketStat.Uid != uint32(os.Geteuid()) || socketStat.Nlink != 1 {
		_ = listener.Close()
		return errors.New("seed socket unavailable")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var clients sync.WaitGroup
	acceptDone := make(chan struct{})
	go acceptClients(ctx, listener, &clients, acceptDone)
	command := exec.Command(seedPath) // #nosec G204 -- fixed seed helper supplied in the immutable tools image.
	command.Env = []string{}
	command.Stdin = os.Stdin
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	commandErr := command.Run()
	cancel()
	closeErr := listener.Close()
	<-acceptDone
	drained := make(chan struct{})
	go func() {
		clients.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		return errors.New("seed connections did not drain")
	}
	if closeErr != nil {
		return errors.New("seed socket cleanup failed")
	}
	if err := removeOwnedSocket(socketPath, socketInfo); err != nil {
		return err
	}
	if commandErr != nil {
		return errors.New("synthetic fixture account creation failed")
	}
	return nil
}

func acceptClients(ctx context.Context, listener *net.UnixListener, clients *sync.WaitGroup, done chan<- struct{}) {
	defer close(done)
	var active sync.Mutex
	var count int
	connections := make(map[net.Conn]struct{})
	go func() {
		<-ctx.Done()
		active.Lock()
		defer active.Unlock()
		for connection := range connections {
			_ = connection.Close()
		}
	}()
	for {
		local, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		active.Lock()
		if count >= maxClients || ctx.Err() != nil {
			active.Unlock()
			_ = local.Close()
			continue
		}
		count++
		connections[local] = struct{}{}
		clients.Add(1)
		active.Unlock()
		go func() {
			defer clients.Done()
			defer func() {
				active.Lock()
				count--
				delete(connections, local)
				active.Unlock()
			}()
			proxyConnection(local, database)
		}()
	}
}

func proxyConnection(local net.Conn, address string) {
	defer local.Close()
	remote, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		return
	}
	defer remote.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(remote, local)
		if half, ok := remote.(*net.TCPConn); ok {
			_ = half.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, remote)
		if half, ok := local.(*net.UnixConn); ok {
			_ = half.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

func removeOwnedSocket(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("seed socket cleanup failed")
	}
	currentStat, currentOK := current.Sys().(*syscall.Stat_t)
	expectedStat, expectedOK := expected.Sys().(*syscall.Stat_t)
	if !currentOK || !expectedOK || current.Mode()&os.ModeSocket == 0 ||
		currentStat.Dev != expectedStat.Dev || currentStat.Ino != expectedStat.Ino ||
		currentStat.Uid != uint32(os.Geteuid()) || currentStat.Nlink != 1 {
		return errors.New("seed socket ownership changed")
	}
	if err := unix.Unlink(path); err != nil {
		return errors.New("seed socket cleanup failed")
	}
	return nil
}
