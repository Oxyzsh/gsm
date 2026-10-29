package modem

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go-pess/platform/user/pess/gsm/internal/at"
)

type Status int

const (
	StatusDisabled Status = iota
	StatusNoHW
	StatusNoCard
	StatusNoSignal
	StatusDialFail
	StatusBusy
	StatusOK
	StatusOperationFailed
)

func (s Status) String() string {
	switch s {
	case StatusDisabled:
		return "HW Disabled"
	case StatusNoHW:
		return "No hardware"
	case StatusNoCard:
		return "No sim-card"
	case StatusNoSignal:
		return "No signal"
	case StatusDialFail:
		return "Dial fail"
	case StatusBusy:
		return "Busy"
	case StatusOK:
		return "OK"
	case StatusOperationFailed:
		return "Operation failed"
	default:
		return "Unknown"
	}
}

func (s Status) Detail() string {
	switch s {
	case StatusDisabled:
		return "module is disabled"
	case StatusNoHW:
		return "serial port unavailable or module did not respond"
	case StatusNoCard:
		return "sim card not inserted"
	case StatusNoSignal:
		return "signal is too weak"
	case StatusDialFail:
		return "dial failed"
	case StatusBusy:
		return "module busy"
	case StatusOK:
		return "module is ready"
	case StatusOperationFailed:
		return "AT operation failed"
	default:
		return "unknown module status"
	}
}

type Transport interface {
	Do(ctx context.Context, cmd string) (at.Reply, error)
	WaitPrompt(ctx context.Context, cmd string) error
	SendRaw(ctx context.Context, data []byte) (at.Reply, error)
}

type Reopener interface {
	Reopen(ctx context.Context) error
}

type Controller interface {
	Status(ctx context.Context) (StatusResult, error)
	SetEnabled(ctx context.Context, enabled bool) (StatusResult, error)
	SendSMS(ctx context.Context, number, message string) (SendResult, error)
	SendVoice(ctx context.Context, number, message string) (StatusResult, error)
	Redial(ctx context.Context) error
	Reinitialize(ctx context.Context) error
}

type StatusResult struct {
	Status    Status
	Enabled   bool
	Detail    string
	RSSI      int
	BER       int
	HWType    string
	Firmware  string
	CheckedAt time.Time
}

type SendResult struct {
	Status   Status
	Detail   string
	Segments int
}

type Config struct {
	APN         string
	BaudRate    int
	ModuleType  string
	VersionFile string
	// VersionFileWrite 为 true 时，识别到模块后把版本号写回 VersionFile（自维护缓存）。
	VersionFileWrite bool
	// DataMode 是 module.data_mode 的取值：auto / at / external。
	// auto 时按 AT+QCFG="usbnet" 判断数据面模式，QMI(0) 交给外部拨号工具。
	DataMode     string
	Logger       *slog.Logger
	PollInterval time.Duration
	BusyInterval time.Duration
}

var (
	ErrVoiceUnsupported = errors.New("voice is unsupported for this module")
	ErrOperationFailed  = errors.New("AT operation failed")
)

type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Reason)
}

type controller struct {
	transport Transport
	reopener  Reopener
	cfg       Config
	logger    *slog.Logger

	opMu chan struct{}

	mu                 sync.Mutex
	enabled            bool
	hwType             string
	firmware           string
	detectionAttempted bool
	dataMode           string // "" 未判定；"at" 本服务拨；"external" 外部工具拨
	ttsChecked         bool
	apnResolved        string // apn=auto 时按 IMSI 推出来的 APN（空=还没读到卡）
	apnApplied         string // 已经下发给模块的 APN，避免每次轮询重复下发
	lastStatus         StatusResult
}

var (
	csqPattern   = regexp.MustCompile(`(?i)\+CSQ:\s*(\d+)\s*,\s*(\d+)`)
	cpinPattern  = regexp.MustCompile(`(?i)^\+CPIN:\s*READY\s*$`)
	cpasPattern  = regexp.MustCompile(`(?i)\+CPAS:\s*(\d+)`)
	qwttsPattern = regexp.MustCompile(`(?i)\+QWTTS:\s*0(?:\s|$)`)

	// qcfgUsbnetPattern 解析 AT+QCFG="usbnet" 的数据面模式：
	// 0=QMI（数据由 QMI 拨号工具负责）、1=ECM、2=MBIM、3=RNDIS。
	qcfgUsbnetPattern = regexp.MustCompile(`(?i)\+QCFG:\s*"usbnet"\s*,\s*(\d+)`)
	// qttsetupPattern 解析 +QTTSETUP: 2,3,<n>，<n>=0 表示当前为中文语音库。
	qttsetupPattern = regexp.MustCompile(`(?i)\+QTTSETUP:\s*\d+\s*,\s*\d+\s*,\s*(-?\d+)`)
)

func NewController(transport Transport, reopener Reopener, cfg Config) Controller {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.BusyInterval <= 0 {
		cfg.BusyInterval = time.Second
	}
	c := &controller{
		transport: transport,
		reopener:  reopener,
		cfg:       cfg,
		logger:    cfg.Logger,
		opMu:      make(chan struct{}, 1),
		enabled:   true,
		lastStatus: StatusResult{
			Status:    StatusOK,
			Enabled:   true,
			Detail:    StatusOK.Detail(),
			RSSI:      -1,
			BER:       -1,
			CheckedAt: time.Now(),
		},
	}
	c.hwType, c.firmware = c.configuredIdentity()
	c.lastStatus.HWType = c.hwType
	c.lastStatus.Firmware = c.firmware
	return c
}

func (c *controller) Status(ctx context.Context) (StatusResult, error) {
	if err := c.acquire(ctx); err != nil {
		return StatusResult{Status: StatusOperationFailed, Enabled: c.isEnabled(), HWType: c.moduleType(), Detail: "request canceled"}, err
	}
	defer c.release()
	return c.statusLocked(ctx)
}

func (c *controller) SetEnabled(ctx context.Context, enabled bool) (StatusResult, error) {
	if err := c.acquire(ctx); err != nil {
		return c.currentResult(StatusOperationFailed, "request canceled"), err
	}
	defer c.release()

	// 先判定数据面归属，避免在 QMI 模式下把 AT 拨号命令发出去。
	c.resolveDataModeLocked(ctx)

	c.mu.Lock()
	already := c.enabled == enabled
	c.enabled = enabled
	c.lastStatus.Enabled = enabled
	result := c.lastStatus
	c.mu.Unlock()
	if already {
		result.CheckedAt = time.Now()
		return result, nil
	}

	if err := c.resetLocked(ctx); err != nil {
		result = c.currentResult(StatusOperationFailed, err.Error())
		c.cacheStatus(result)
		return result, nil
	}
	if enabled {
		_ = c.redialLocked(ctx, true)
	} else {
		_ = c.redialLocked(ctx, false)
	}
	if ctx.Err() != nil {
		return c.currentResult(StatusOperationFailed, "request canceled"), ctx.Err()
	}
	result, err := c.statusLocked(ctx)
	if err != nil {
		return result, err
	}
	return result, nil
}

func (c *controller) Redial(ctx context.Context) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	c.resolveDataModeLocked(ctx)
	return c.redialLocked(ctx, true)
}

func (c *controller) Reinitialize(ctx context.Context) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	c.refreshConfiguredIdentity()
	c.mu.Lock()
	c.detectionAttempted = false
	c.mu.Unlock()
	if c.reopener == nil {
		return errors.New("serial reopen is not configured")
	}
	return c.reopener.Reopen(ctx)
}

func (c *controller) statusLocked(ctx context.Context) (StatusResult, error) {
	hwType, firmware := c.identity()
	result := StatusResult{
		Status:    StatusOK,
		Enabled:   c.isEnabled(),
		RSSI:      -1,
		BER:       -1,
		HWType:    hwType,
		Firmware:  firmware,
		CheckedAt: time.Now(),
	}
	if !result.Enabled {
		result.Status = StatusDisabled
		result.Detail = StatusDisabled.Detail()
		c.cacheStatus(result)
		return result, nil
	}

	if _, err := c.command(ctx, "AT", 3*time.Second); err != nil {
		if isContextError(err) {
			return result, err
		}
		result.Status = StatusNoHW
		result.Detail = StatusNoHW.Detail()
		c.cacheStatus(result)
		return result, nil
	}
	c.detectFromATLocked(ctx)
	result.HWType, result.Firmware = c.identity()
	c.resolveDataModeLocked(ctx)

	cpin, err := c.command(ctx, "AT+CPIN?", 3*time.Second)
	if err != nil || !cpinReady(cpin.Lines) {
		if isContextError(err) {
			return result, err
		}
		result.Status = StatusNoCard
		result.Detail = StatusNoCard.Detail()
		c.cacheStatus(result)
		return result, nil
	}

	c.ensureTTSLocked(ctx)
	c.ensureAPNLocked(ctx)

	csq, err := c.command(ctx, "AT+CSQ", 3*time.Second)
	if err != nil {
		if isContextError(err) {
			return result, err
		}
		c.logger.Warn("AT+CSQ failed; continuing status check", "error", err)
	} else if match := csqPattern.FindStringSubmatch(strings.Join(csq.Lines, "\n")); len(match) == 3 {
		rssi, _ := strconv.Atoi(match[1])
		ber, _ := strconv.Atoi(match[2])
		result.RSSI = rssi
		result.BER = ber
		if rssi <= 16 || rssi == 99 {
			result.Status = StatusNoSignal
			result.Detail = StatusNoSignal.Detail()
			c.cacheStatus(result)
			return result, nil
		}
		c.logger.Debug("signal quality", "rssi", rssi, "ber", ber)
	} else {
		c.logger.Warn("AT+CSQ reply could not be parsed; continuing status check", "raw", csq.Raw)
	}

	result.Status = StatusOK
	result.Detail = StatusOK.Detail()
	c.cacheStatus(result)
	return result, nil
}

func (c *controller) resetLocked(ctx context.Context) error {
	profile := c.profile()
	moduleReset := false
	for _, command := range profile.ResetCommands {
		reply, err := c.command(ctx, command, 3*time.Second)
		if err == nil && reply.OK() {
			moduleReset = true
			c.logger.Info("module reset command accepted", "command", command, "module", profile.Type)
			break
		}
		if isContextError(err) {
			return err
		}
		c.logger.Warn("module reset command failed; trying next fallback", "command", command, "error", err, "raw", reply.Raw)
	}
	if moduleReset {
		if c.reopener == nil {
			return errors.New("serial reopen is not configured")
		}
		waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err := c.reopener.Reopen(waitCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("wait for module recovery: %w", err)
		}
	} else {
		c.logger.Warn("all module reset commands failed; continuing best-effort reinitialization", "module", profile.Type)
	}

	commands := []string{
		"ATE0",
	}
	if apn := c.resetAPNLocked(); apn != "" {
		commands = append(commands, fmt.Sprintf(`AT+cgdcont=1,"IP","%s"`, apn))
	}
	// 复位后模块重新读卡，IMSI/APN 缓存要作废，下次状态检查重新解析。
	c.apnResolved = ""
	c.apnApplied = ""
	if !c.dataDialingExternal() {
		commands = append(commands, profile.ExtraResetCommands...)
	}
	commands = append(commands, fmt.Sprintf("AT+ipr=%d", c.cfg.BaudRate))
	for _, command := range commands {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := c.command(ctx, command, 3*time.Second); err != nil {
			c.logger.Warn("best-effort reset command failed", "command", command, "error", err)
		}
	}
	return nil
}

func (c *controller) redialLocked(ctx context.Context, enabled bool) error {
	if c.dataDialingExternal() {
		c.logger.Info("data dialing is delegated to the external dialer; skip AT data commands", "enabled", enabled)
		return nil
	}
	profile := c.profile()
	commands := []string{profile.DataDownCommand}
	if enabled && profile.DataUpCommand != "" {
		commands = append(commands, profile.DataUpCommand)
	}
	var firstErr error
	for _, command := range commands {
		if command == "" {
			continue
		}
		if _, err := c.command(ctx, command, 3*time.Second); err != nil {
			if isContextError(err) {
				return err
			}
			c.logger.Warn("best-effort redial command failed", "command", command, "module", profile.Type, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// dataDialingExternal 报告数据链路是否由外部拨号工具负责（QMI 模式）。
// 该判断决定 SetEnabled/保活重拨要不要发 AT 拨号命令：QMI 模式下 AT+cgact 之类
// 不是有效路径，发了只会得到 ERROR 或被忽略，真正拨号由 quectel-CM 完成。
func (c *controller) dataDialingExternal() bool {
	c.mu.Lock()
	mode := c.dataMode
	c.mu.Unlock()
	if mode == "" {
		// 尚未探测时先认配置：显式 external 的部署不必等一次状态检查。
		return strings.EqualFold(strings.TrimSpace(c.cfg.DataMode), "external")
	}
	return mode == "external"
}

// resolveDataModeLocked 在首次状态检查时判定数据面由谁拨号，判定结果缓存。
func (c *controller) resolveDataModeLocked(ctx context.Context) {
	c.mu.Lock()
	resolved := c.dataMode
	c.mu.Unlock()
	if resolved != "" {
		return
	}
	mode := strings.ToLower(strings.TrimSpace(c.cfg.DataMode))
	if mode == "" {
		mode = "auto"
	}
	if mode == "at" || mode == "external" {
		c.setDataMode(mode, "configured by module.data_mode")
		return
	}
	// auto：只有移远 EM05 系列用 AT+QCFG="usbnet" 表达数据面模式。
	if moduleType := c.moduleType(); moduleType != ModuleEM05CE && moduleType != ModuleEM05CN {
		c.setDataMode("at", "module has no usbnet configuration")
		return
	}
	reply, err := c.command(ctx, `AT+QCFG="usbnet"`, 3*time.Second)
	if err != nil {
		if isContextError(err) {
			return // 交给下一次状态检查
		}
		c.logger.Warn("AT+QCFG=\"usbnet\" failed; assume AT data dialing", "error", err)
		c.setDataMode("at", "usbnet query failed")
		return
	}
	match := qcfgUsbnetPattern.FindStringSubmatch(strings.Join(reply.Lines, "\n"))
	if len(match) != 2 {
		c.logger.Warn("AT+QCFG=\"usbnet\" reply could not be parsed; assume AT data dialing", "raw", reply.Raw)
		c.setDataMode("at", "usbnet reply unparsable")
		return
	}
	if match[1] == "0" {
		c.setDataMode("external", `usbnet=0 (QMI): data dialing belongs to the external dialer`)
		return
	}
	c.setDataMode("at", fmt.Sprintf("usbnet=%s", match[1]))
}

func (c *controller) setDataMode(mode, reason string) {
	c.mu.Lock()
	changed := c.dataMode != mode
	c.dataMode = mode
	c.mu.Unlock()
	if changed {
		c.logger.Info("data mode resolved", "mode", mode, "reason", reason)
	}
}

// ensureTTSLocked 确认移远模块的中文语音库在位；不在位时切库并提示需要重启。
// 只检查一次，避免每次状态查询都发 AT 命令。
func (c *controller) ensureTTSLocked(ctx context.Context) {
	c.mu.Lock()
	checked := c.ttsChecked
	c.mu.Unlock()
	if checked || !c.profile().TTSSetup {
		return
	}
	reply, err := c.command(ctx, "AT+QTTSETUP=2,3", 3*time.Second)
	if err != nil {
		if isContextError(err) {
			return
		}
		c.logger.Debug("AT+QTTSETUP query failed; skip TTS library check", "error", err)
		return
	}
	c.mu.Lock()
	c.ttsChecked = true
	c.mu.Unlock()
	match := qttsetupPattern.FindStringSubmatch(strings.Join(reply.Lines, "\n"))
	if len(match) != 2 {
		c.logger.Warn("AT+QTTSETUP reply could not be parsed", "raw", reply.Raw)
		return
	}
	if strings.EqualFold(strings.TrimSpace(match[1]), "0") {
		c.logger.Debug("chinese TTS library is active")
		return
	}
	c.logger.Warn("module TTS library is not chinese; switching to chinese library", "current", match[1])
	if _, err := c.command(ctx, "AT+QTTSETUP=1,3,0", 3*time.Second); err != nil {
		c.logger.Warn("switch to chinese TTS library failed", "error", err)
		return
	}
	c.logger.Info("chinese TTS library selected; a module reset (AT+CFUN=1,1) is required before it takes effect")
}

func (c *controller) checkBusyLocked(ctx context.Context) (bool, error) {
	if !c.profile().BusyCheck {
		return false, nil
	}
	busy := false
	for attempt := 0; attempt < 2; attempt++ {
		reply, err := c.command(ctx, "AT+CPAS", 3*time.Second)
		if err != nil {
			if isContextError(err) {
				return false, err
			}
			return false, fmt.Errorf("%w: AT+CPAS: %v", ErrOperationFailed, err)
		}
		match := cpasPattern.FindStringSubmatch(strings.Join(reply.Lines, "\n"))
		if len(match) != 2 {
			return false, fmt.Errorf("%w: cannot parse AT+CPAS reply", ErrOperationFailed)
		}
		state, _ := strconv.Atoi(match[1])
		if state != 0 {
			busy = true
		}
		if attempt == 0 {
			if err := wait(ctx, c.cfg.BusyInterval); err != nil {
				return false, err
			}
		}
	}
	return busy, nil
}

func (c *controller) command(ctx context.Context, cmd string, timeout time.Duration) (at.Reply, error) {
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.transport.Do(commandCtx, cmd)
}

func (c *controller) acquire(ctx context.Context) error {
	select {
	case c.opMu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *controller) release() {
	<-c.opMu
}

func (c *controller) configuredIdentity() (string, string) {
	moduleType, firmware := normalizeSelection(c.cfg.ModuleType)
	content, err := os.ReadFile(c.cfg.VersionFile)
	if err != nil {
		if moduleType == "unknown" {
			c.logger.Warn("cannot read module version file; module identity will be probed through ATI", "path", c.cfg.VersionFile, "error", err)
		}
		return moduleType, firmware
	}
	detectedType, detectedFirmware, ok := identifyModule(string(content))
	if !ok {
		c.logger.Warn("module version file is not recognized; module identity will be probed through ATI", "path", c.cfg.VersionFile)
		return moduleType, firmware
	}
	if moduleType == "unknown" {
		return detectedType, detectedFirmware
	}
	if detectedType == moduleType && firmware == FirmwareUnknown && detectedFirmware != FirmwareUnknown {
		firmware = detectedFirmware
	}
	return moduleType, firmware
}

func (c *controller) refreshConfiguredIdentity() {
	moduleType, firmware := c.configuredIdentity()
	c.mu.Lock()
	c.hwType = moduleType
	c.firmware = firmware
	c.lastStatus.HWType = moduleType
	c.lastStatus.Firmware = firmware
	c.mu.Unlock()
}

func (c *controller) identity() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hwType, c.firmware
}

func (c *controller) profile() moduleProfile {
	moduleType, firmware := c.identity()
	return profileFor(moduleType, firmware)
}

func (c *controller) needsDetection() bool {
	c.mu.Lock()
	if c.detectionAttempted {
		c.mu.Unlock()
		return false
	}
	moduleType := c.hwType
	firmware := c.firmware
	c.mu.Unlock()

	// auto（或未配置）时永远按 AT 实测一次：版本文件只是上一次的缓存，
	// 换过模块之后文件可能是旧的，必须以模块自己报的为准。
	configured := strings.TrimSpace(c.cfg.ModuleType)
	if configured == "" || strings.EqualFold(configured, "auto") {
		return true
	}
	if moduleType == "unknown" {
		return true
	}
	if (moduleType == ModuleEM05CN || moduleType == ModuleFibocom) && firmware == FirmwareUnknown {
		return true
	}
	return false
}

func (c *controller) detectFromATLocked(ctx context.Context) {
	if !c.needsDetection() {
		return
	}
	var identityText strings.Builder
	receivedIdentityReply := false
	revision := ""
	for _, command := range []string{"ATI", "AT+CGMM", "AT+CGMR"} {
		reply, err := c.command(ctx, command, 3*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.logger.Debug("module identity command failed", "command", command, "error", err)
			continue
		}
		receivedIdentityReply = true
		identityText.WriteString(reply.Raw)
		identityText.WriteByte('\n')
		identityText.WriteString(strings.Join(reply.Lines, "\n"))
		identityText.WriteByte('\n')
		if revision == "" {
			revision = revisionFromReply(command, reply)
		}
	}
	if !receivedIdentityReply {
		return
	}
	c.mu.Lock()
	c.detectionAttempted = true
	c.mu.Unlock()
	detectedType, detectedFirmware, ok := identifyModule(identityText.String())
	if !ok {
		c.logger.Warn("module identity could not be determined from ATI/AT+CGMM/AT+CGMR", "raw", identityText.String())
		return
	}

	c.applyDetectedIdentity(detectedType, detectedFirmware)
	if revision != "" {
		c.writeVersionFile(revision)
	}
}

// applyDetectedIdentity 用 AT 实测结果修正身份。
//
// module.type 为 auto（或未配置）时以实测为准：版本文件里可能留着换下来的旧模块信息，
// 只信文件会一路错下去（比如把 EM05 当成 L610，发错厂商专用命令）。
// 显式配置了 module.type 时绝不覆盖，那是运维的强制指定。
func (c *controller) applyDetectedIdentity(detectedType, detectedFirmware string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	configured := strings.TrimSpace(c.cfg.ModuleType)
	autoMode := configured == "" || strings.EqualFold(configured, "auto")
	previousType, previousFirmware := c.hwType, c.firmware

	switch {
	case autoMode:
		if previousType != "" && previousType != "unknown" && previousType != detectedType {
			c.logger.Warn("module identity in version file disagrees with AT reply; trusting AT",
				"version_file", previousType, "at_reply", detectedType)
		}
		c.hwType = detectedType
		c.firmware = detectedFirmware
	case previousType == "unknown":
		c.hwType = detectedType
		c.firmware = detectedFirmware
	case previousType == detectedType && previousFirmware == FirmwareUnknown && detectedFirmware != FirmwareUnknown:
		c.firmware = detectedFirmware
	}
	c.lastStatus.HWType = c.hwType
	c.lastStatus.Firmware = c.firmware
}

// revisionFromReply 从 ATI/AT+CGMR 的应答里取版本号字符串。
//
// ATI 只认 "Revision:" 前缀（否则会把 "Fibocom"、"Quectel" 这种厂商名当成版本号，
// L610 的 ATI 第一行就是 Fibocom）；裸版本号只从 AT+CGMR 取。
func revisionFromReply(command string, reply at.Reply) string {
	for _, line := range reply.Lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.EqualFold(line, "OK") {
			continue
		}
		if index := strings.Index(strings.ToLower(line), "revision:"); index == 0 {
			return strings.TrimSpace(line[len("revision:"):])
		}
	}
	if !strings.EqualFold(strings.TrimSpace(command), "AT+CGMR") {
		return ""
	}
	for _, line := range reply.Lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.EqualFold(line, "OK") || strings.Contains(strings.ToLower(line), "revision:") {
			continue
		}
		return line
	}
	return ""
}

// writeVersionFile 把实测版本号写回版本文件，让厂商那个会抢 AT 口的脚本可以永久停用。
// 写入失败只记录一次警告：文件所在目录不可写（未配 ReadWritePaths/chown）时服务照常工作。
func (c *controller) writeVersionFile(revision string) {
	if !c.cfg.VersionFileWrite || strings.TrimSpace(c.cfg.VersionFile) == "" {
		return
	}
	current, err := os.ReadFile(c.cfg.VersionFile)
	if err == nil && strings.TrimSpace(string(current)) == revision {
		return
	}
	if err := os.WriteFile(c.cfg.VersionFile, []byte(revision+"\n"), 0o644); err != nil {
		c.logger.Warn("cannot update module version file; module identity is still detected over AT",
			"path", c.cfg.VersionFile, "error", err)
		return
	}
	c.logger.Info("module version file updated", "path", c.cfg.VersionFile, "content", revision)
}

func (c *controller) isEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled
}

func (c *controller) moduleType() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hwType
}

func (c *controller) cacheStatus(result StatusResult) {
	c.mu.Lock()
	c.lastStatus = result
	c.mu.Unlock()
}

func (c *controller) currentResult(status Status, detail string) StatusResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return StatusResult{
		Status:    status,
		Enabled:   c.enabled,
		Detail:    detail,
		RSSI:      c.lastStatus.RSSI,
		BER:       c.lastStatus.BER,
		HWType:    c.hwType,
		Firmware:  c.firmware,
		CheckedAt: time.Now(),
	}
}

func cpinReady(lines []string) bool {
	for _, line := range lines {
		if cpinPattern.MatchString(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
