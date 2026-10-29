package at

import (
	"errors"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
)

// DeviceAuto 让 gsmd 自己找 AT 口，换机器不用改配置。
const DeviceAuto = "auto"

// byIDDir 是 udev 的稳定设备名目录（写成变量便于测试替换）。
var byIDDir = "/dev/serial/by-id"

// atInterfaceSuffix 是 AT 命令口的稳定名后缀。
//
// 移远 EM05 和广和通 L610 的 USB 复合设备里 AT 口都挂在 if02：
// EM05 上是 /dev/ttyUSB2，L610 上是 /dev/ttyUSB0 —— ttyUSB 编号随机器变，
// 但 "usb-<厂商>_<型号>-if02-port0" 这个符号链接跟着设备走，不会错位。
const atInterfaceSuffix = "-if02-port0"

// vendorHints 用于在同一台机器插了多个模块时优先挑常见的蜂窝模块。
var vendorHints = []string{"Quectel", "FIBOCOM", "SIMCOM", "NEOWAY", "MEIG", "ZTE"}

// ResolveDevice 返回要打开的串口设备。
//
// 显式路径原样返回；auto 时先按稳定名找 AT 口，找不到再退回第一个 /dev/ttyUSB*。
func ResolveDevice(configured string, logger *slog.Logger) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" && !strings.EqualFold(configured, DeviceAuto) {
		return configured, nil
	}
	if device, ok := byIDATPort(); ok {
		if logger != nil {
			logger.Info("AT port resolved by stable id", "device", device)
		}
		return device, nil
	}
	if device, ok := firstUSBTTY(); ok {
		if logger != nil {
			logger.Warn("stable AT port symlink not found; falling back to first ttyUSB", "device", device)
		}
		return device, nil
	}
	return "", errors.New("no AT port found: neither " + byIDDir + "/*" + atInterfaceSuffix + " nor /dev/ttyUSB0 exists")
}

func byIDATPort() (string, bool) {
	entries, err := filepath.Glob(filepath.Join(byIDDir, "*"+atInterfaceSuffix))
	if err != nil || len(entries) == 0 {
		return "", false
	}
	sort.Strings(entries)
	for _, hint := range vendorHints {
		for _, entry := range entries {
			if strings.Contains(filepath.Base(entry), hint) {
				return entry, true
			}
		}
	}
	return entries[0], true
}

func firstUSBTTY() (string, bool) {
	entries, err := filepath.Glob("/dev/ttyUSB*")
	if err != nil || len(entries) == 0 {
		return "", false
	}
	sort.Slice(entries, func(i, j int) bool { return usbTTYIndex(entries[i]) < usbTTYIndex(entries[j]) })
	return entries[0], true
}

// usbTTYIndex 取 ttyUSB 后面的编号，保证 ttyUSB10 排在 ttyUSB9 之后。
func usbTTYIndex(path string) int {
	index := 0
	for _, r := range filepath.Base(path) {
		if r < '0' || r > '9' {
			index = 0
			continue
		}
		index = index*10 + int(r-'0')
	}
	return index
}
