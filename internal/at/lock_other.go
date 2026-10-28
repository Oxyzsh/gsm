//go:build !linux

package at

// lockDevice 在非 Linux 平台是空实现：开发机（Windows/macOS）没有
// /dev/ttyUSB* 的跨进程独占语义，单元测试也不该依赖内核文件锁。
func lockDevice(string) (func() error, error) {
	return func() error { return nil }, nil
}
