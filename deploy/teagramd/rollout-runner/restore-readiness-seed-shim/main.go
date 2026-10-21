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
	"sync/atomic"
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
	if !ok || stat.Uid != 0 {
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
		if closeErr := listener.Close(); closeErr != nil {
			return errors.New("seed socket unavailable")
		}
		return errors.New("seed socket unavailable")
	}
	socketStat, ok := socketInfo.Sys().(*syscall.Stat_t)
	if !ok || socketStat.Uid != 0 || socketStat.Nlink != 1 {
		if closeErr := listener.Close(); closeErr != nil {
			return errors.New("seed socket unavailable")
		}
		return errors.New("seed socket unavailable")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var clients sync.WaitGroup
	var proxyFailed atomic.Bool
	acceptDone := make(chan struct{})
	go acceptClients(ctx, listener, &clients, acceptDone, &proxyFailed)
	command := exec.CommandContext(ctx, seedPath) // #nosec G204 -- fixed seed helper supplied in the immutable tools image.
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
	if closeErr != nil || proxyFailed.Load() {
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

func acceptClients(ctx context.Context, listener *net.UnixListener, clients *sync.WaitGroup, done chan<- struct{}, proxyFailed *atomic.Bool) {
	defer close(done)
	var active sync.Mutex
	var count int
	connections := make(map[net.Conn]struct{})
	go func() {
		<-ctx.Done()
		active.Lock()
		defer active.Unlock()
		for connection := range connections {
			if err := connection.Close(); err != nil {
				proxyFailed.Store(true)
			}
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
			if err := local.Close(); err != nil {
				proxyFailed.Store(true)
			}
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
			if err := proxyConnection(ctx, local, database); err != nil && ctx.Err() == nil {
				proxyFailed.Store(true)
			}
		}()
	}
}

func proxyConnection(ctx context.Context, local net.Conn, address string) (proxyErr error) {
	defer func() {
		if err := local.Close(); err != nil {
			proxyErr = errors.Join(proxyErr, errors.New("local proxy connection close failed"))
		}
	}()
	remote, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return errors.New("scratch database connection failed")
	}
	defer func() {
		if err := remote.Close(); err != nil {
			proxyErr = errors.Join(proxyErr, errors.New("remote proxy connection close failed"))
		}
	}()
	done := make(chan error, 2)
	go func() {
		_, copyErr := io.Copy(remote, local)
		var closeErr error
		if half, ok := remote.(*net.TCPConn); ok {
			closeErr = half.CloseWrite()
		}
		done <- errors.Join(copyErr, closeErr)
	}()
	go func() {
		_, copyErr := io.Copy(local, remote)
		var closeErr error
		if half, ok := local.(*net.UnixConn); ok {
			closeErr = half.CloseWrite()
		}
		done <- errors.Join(copyErr, closeErr)
	}()
	proxyErr = errors.Join(<-done, <-done)
	return proxyErr
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
		currentStat.Uid != 0 || currentStat.Nlink != 1 {
		return errors.New("seed socket ownership changed")
	}
	if err := unix.Unlink(path); err != nil {
		return errors.New("seed socket cleanup failed")
	}
	return nil
}
