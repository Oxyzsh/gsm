//go:build linux

package at

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockDevice 给 AT 设备加跨进程独占锁，返回释放函数。
//
// 用 flock 而不是 TIOCEXCL：TIOCEXCL 对带 CAP_SYS_ADMIN 的进程不生效，
// 现场脚本多以 root 直接 cat 串口，锁不住；flock 至少能约束所有遵守同一约定的
// 持有者（gsmd 自身重启、调试工具、按文档加锁的脚本），拿不到锁时 fail fast。
func lockDevice(path string) (func() error, error) {
	file, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s for exclusive lock: %w", path, err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrDeviceBusy, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() error {
		unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
		closeErr := file.Close()
		if unlockErr != nil {
			return unlockErr
		}
		return closeErr
	}, nil
}
