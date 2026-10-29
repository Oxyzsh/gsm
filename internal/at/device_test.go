package at

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDeviceKeepsExplicitPath(t *testing.T) {
	device, err := ResolveDevice("/dev/ttyUSB3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if device != "/dev/ttyUSB3" {
		t.Fatalf("device=%q want=/dev/ttyUSB3", device)
	}
}

func TestResolveDevicePrefersStableATPort(t *testing.T) {
	dir := t.TempDir()
	previous := byIDDir
	byIDDir = dir
	defer func() { byIDDir = previous }()

	// 同一台机器上可能挂多个 USB 串口设备，蜂窝模块要优先。
	for _, name := range []string{"usb-CH9344_USB-if02-port0", "usb-FIBOCOM_L610-if02-port0"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	device, err := ResolveDevice("auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(device) != "usb-FIBOCOM_L610-if02-port0" {
		t.Fatalf("device=%q want the Fibocom AT port", device)
	}
}

func TestResolveDeviceFallsBackWhenNoStablePort(t *testing.T) {
	dir := t.TempDir()
	previous := byIDDir
	byIDDir = dir
	defer func() { byIDDir = previous }()

	// 稳定名目录里只有非 AT 口（if00），应退回 /dev/ttyUSB*。
	if err := os.WriteFile(filepath.Join(dir, "usb-QUECTEL_EM05-if00-port0"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	device, err := ResolveDevice("auto", nil)
	if err != nil {
		// 本机（开发机）没有 /dev/ttyUSB*，这种情况只要求不 panic、报错清晰。
		t.Skipf("no /dev/ttyUSB* on this host: %v", err)
	}
	if filepath.Dir(device) != "/dev" {
		t.Fatalf("device=%q want /dev/ttyUSB*", device)
	}
}

func TestUSBTTYIndexOrdersNumerically(t *testing.T) {
	if usbTTYIndex("/dev/ttyUSB10") <= usbTTYIndex("/dev/ttyUSB9") {
		t.Fatal("ttyUSB10 must sort after ttyUSB9")
	}
}
