package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// deployLockName is the lock file in <deploy.root_dir>/<brand>: the service (webhook, watch) and bootstrap run as
// separate processes and must never deploy at the same time.
const deployLockName = ".deputy.lock"

// lockDeploys serializes deploys inside this process (s.mu) and across processes (flock on the lock file). It
// waits for a running deploy and returns the function that releases both.
func (s *Server) lockDeploys() (func(), error) {
	s.mu.Lock()
	dir := filepath.Join(s.cfg.Deploy.RootDir, s.cfg.Deploy.DefaultBrand)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("deploy lock: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, deployLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("deploy lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		s.mu.Unlock()
		return nil, fmt.Errorf("deploy lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		s.mu.Unlock()
	}, nil
}
