//go:build darwin || linux

package monitor

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

type ServiceLock struct {
	file *os.File
}

func AcquireServiceLock(dbPath string) (*ServiceLock, error) {
	path, err := filepath.Abs(dbPath + ".service.lock")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		file.Close()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("service is running")
		}
		return nil, err
	}
	return &ServiceLock{file: file}, nil
}

func (l *ServiceLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}

func ValidatePrivateDB(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o600 {
		return errors.New("database permissions must be 0600")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("database must be owned by current service user")
	}
	parent, err := os.Stat(filepath.Dir(abs))
	if err != nil {
		return err
	}
	if parent.Mode().Perm()&0o077 != 0 {
		return errors.New("database directory must be 0700 or stricter")
	}
	return nil
}

func ValidateEnvironmentFile(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 || mode&0o400 == 0 || mode&0o111 != 0 {
		return errors.New("environment file permissions must be owner-readable and inaccessible to group/other")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("environment file must be owned by current service user")
	}
	return nil
}
