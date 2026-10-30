package modem

import "strings"

const (
	ModuleAuto      = "auto"
	ModuleEM05CE    = "em05-ce"
	ModuleEM05CN    = "em05-cn"
	ModuleFibocom   = "fibocom"
	FirmwareA03     = "A03"
	FirmwareA04     = "A04"
	FirmwareL610    = "L610"
	FirmwareUnknown = "unknown"

	// TTS 方言：不同厂商的语音合成命令不一样，业务代码只认方言名，不认具体 AT 串。
	TTSStyleNone  = ""       // 不支持语音播报
	TTSStyleQWTTS = "qwt ts" // 移远：AT+QWTTS=1,1,1,"<UCS2 hex>"，进度用 AT+QWTTS? 查
	TTSStyleGTTS  = "gtts"   // 广和通：AT+GTTS=1,"<明文>",0，进度用 AT+GTTS? 查
)

type moduleProfile struct {
	Type          string
	Firmware      string
	SupportsVoice bool
	// TTSStyle 决定用哪套语音合成命令；为空表示只能拨号不能播报。
	TTSStyle  string
	BusyCheck bool
	// TTSSetup 表示模块支持 AT+QTTSETUP 查询/切换语音合成语言库；
	// 只有移远 EM05 系列有这条命令，广和通不发。
	TTSSetup           bool
	ResetCommands      []string
	ExtraResetCommands []string
	DataDownCommand    string
	DataUpCommand      string
	SMSStorageCommand  string
}

func profileFor(moduleType, firmware string) moduleProfile {
	switch moduleType {
	case ModuleEM05CE, ModuleEM05CN:
		return moduleProfile{
			Type:               moduleType,
			Firmware:           normalizeFirmware(firmware),
			SupportsVoice:      true,
			TTSStyle:           TTSStyleQWTTS,
			BusyCheck:          true,
			TTSSetup:           true,
			ResetCommands:      []string{"AT^reset", "AT+CFUN=1,1"},
			ExtraResetCommands: []string{"AT+qnetdevctl=1,1,1"},
			DataDownCommand:    "AT+cgact=0,1",
			DataUpCommand:      "AT+cgact=1,1",
			SMSStorageCommand:  `AT+QCFG="ltectcc/smsstorage",1`,
		}
	case ModuleFibocom:
		return moduleProfile{
			Type:          ModuleFibocom,
			Firmware:      normalizeFirmware(firmware),
			SupportsVoice: false,
			// L610 固件里有 AT+GTTS，但本版本（16000.1000.00.06.74.05）实测
			// AT+GTTS=1,"<文本>",0/1 恒回 +CME ERROR: parameters are invalid（通话中/外都一样），
			// 参数含义需要 Fibocom 手册确认。代码路径已就绪（TTSStyleGTTS），
			// 确认参数后把 SupportsVoice 改回 true、TTSStyle 设为 TTSStyleGTTS 即可。
			TTSStyle:        TTSStyleNone,
			BusyCheck:       false,
			ResetCommands:   []string{"AT+CFUN=15", "AT+CFUN=1,1"},
			DataDownCommand: "AT+GTRNDIS=0,1",
			DataUpCommand:   "AT+GTRNDIS=1,1",
		}
	default:
		return moduleProfile{
			Type:            "unknown",
			Firmware:        FirmwareUnknown,
			SupportsVoice:   false,
			BusyCheck:       false,
			ResetCommands:   []string{"AT+CFUN=1,1"},
			DataDownCommand: "AT+cgact=0,1",
			DataUpCommand:   "AT+cgact=1,1",
		}
	}
}

// normalizeSelection converts public configuration aliases to the canonical
// module type and firmware variant stored by the controller.
func normalizeSelection(value string) (moduleType, firmware string) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ModuleEM05CE:
		return ModuleEM05CE, ""
	case ModuleEM05CN:
		return ModuleEM05CN, FirmwareUnknown
	case "em05-cn-a03":
		return ModuleEM05CN, FirmwareA03
	case "em05-cn-a04":
		return ModuleEM05CN, FirmwareA04
	case ModuleFibocom:
		return ModuleFibocom, FirmwareUnknown
	case "fibocom-l610", "l610":
		return ModuleFibocom, FirmwareL610
	default:
		return "unknown", FirmwareUnknown
	}
}

func normalizeFirmware(value string) string {
	upper := strings.ToUpper(strings.TrimSpace(value))
	switch {
	case strings.Contains(upper, "A03"):
		return FirmwareA03
	case strings.Contains(upper, "A04"):
		return FirmwareA04
	case strings.Contains(upper, "L610"):
		return FirmwareL610
	case upper == "" || upper == FirmwareUnknown:
		return FirmwareUnknown
	default:
		return upper
	}
}

func identifyModule(text string) (moduleType, firmware string, ok bool) {
	normalized := strings.ToUpper(text)
	compact := strings.NewReplacer("-", "", "_", "", " ", "", "\t", "", "\r", "", "\n", "").Replace(normalized)

	switch {
	case strings.Contains(compact, "EM05CE"):
		return ModuleEM05CE, "", true
	case strings.Contains(compact, "EM05CN"):
		variant := FirmwareUnknown
		switch {
		case strings.Contains(compact, "A03"):
			variant = FirmwareA03
		case strings.Contains(compact, "A04"):
			variant = FirmwareA04
		}
		return ModuleEM05CN, variant, true
	case strings.Contains(compact, "L610"), strings.Contains(compact, "LC610"):
		return ModuleFibocom, FirmwareL610, true
	// L610 的固件版本形如 16000.1000.00.06.74.05，版本文件里只存版本号时也能认出来，
	// 省掉每次开机都去 ATI 探一次（并避免"版本文件无法识别"的告警日志）。
	case strings.HasPrefix(compact, "16000."):
		return ModuleFibocom, FirmwareL610, true
	case strings.Contains(compact, "FIBOCOM"):
		return ModuleFibocom, FirmwareUnknown, true
	default:
		return "", "", false
	}
}
