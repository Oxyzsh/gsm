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
)

type moduleProfile struct {
	Type               string
	Firmware           string
	SupportsVoice      bool
	BusyCheck          bool
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
			Type:            ModuleFibocom,
			Firmware:        normalizeFirmware(firmware),
			SupportsVoice:   false,
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
	case strings.Contains(compact, "FIBOCOM"):
		return ModuleFibocom, FirmwareUnknown, true
	default:
		return "", "", false
	}
}
