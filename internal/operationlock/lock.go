package operationlock

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Release func() error

func Acquire(path string, timeout time.Duration) (Release, error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, writeErr := fmt.Fprintf(f, "%d\n%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
			closeErr := f.Close()
			if writeErr != nil {
				_ = os.Remove(path)
				return nil, writeErr
			}
			if closeErr != nil {
				_ = os.Remove(path)
				return nil, closeErr
			}
			return func() error { return os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if stale(path) {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, errors.New("another Ooh Git operation is still running")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func stale(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid < 1 {
		return true
	}
	err = syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}
