package main

import (
	"fmt"
	"os"
	"syscall"
)

// lockDeploys serializes deploys inside this process (s.mu) and across processes: the service (watch) and
// bootstrap run separately, so they also take an flock on the config file they both read. The file is opened
// read-only (the service sees /etc read-only), and no directory is created for it. Without lockPath (tests) only
// the mutex is taken. It waits for a running deploy and returns the function that releases the lock.
func (s *Server) lockDeploys() (func(), error) {
	s.mu.Lock()
	if s.lockPath == "" {
		return s.mu.Unlock, nil
	}
	f, err := os.Open(s.lockPath)
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("deploy lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		s.mu.Unlock()
		return nil, fmt.Errorf("deploy lock %s: %w", s.lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		s.mu.Unlock()
	}, nil
}
