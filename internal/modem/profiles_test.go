package modem

import "testing"

func TestIdentifyModule(t *testing.T) {
	tests := []struct {
		text     string
		module   string
		firmware string
	}{
		{"Quectel EM05-CE", ModuleEM05CE, ""},
		{"EM05CNFDR08A03M1G_ND_BETA250606", ModuleEM05CN, FirmwareA03},
		{"EM05CNFDR08A04M1G_ND_BETA250904", ModuleEM05CN, FirmwareA04},
		{"Fibocom L610", ModuleFibocom, FirmwareL610},
		{"FIBOCOM-L610-CN", ModuleFibocom, FirmwareL610},
	}
	for _, tc := range tests {
		moduleType, firmware, ok := identifyModule(tc.text)
		if !ok || moduleType != tc.module || firmware != tc.firmware {
			t.Fatalf("identifyModule(%q) = %q, %q, %v", tc.text, moduleType, firmware, ok)
		}
	}
	if _, _, ok := identifyModule("unknown modem"); ok {
		t.Fatal("unknown identity should not be recognized")
	}
}

func TestProfileCommands(t *testing.T) {
	em05 := profileFor(ModuleEM05CN, FirmwareA03)
	if !em05.SupportsVoice || em05.DataUpCommand != "AT+cgact=1,1" || em05.SMSStorageCommand == "" {
		t.Fatalf("unexpected EM05 profile: %+v", em05)
	}
	l610 := profileFor(ModuleFibocom, FirmwareL610)
	if l610.SupportsVoice || l610.DataDownCommand != "AT+GTRNDIS=0,1" || l610.DataUpCommand != "AT+GTRNDIS=1,1" {
		t.Fatalf("unexpected L610 profile: %+v", l610)
	}
	if len(l610.ResetCommands) == 0 || l610.ResetCommands[0] != "AT+CFUN=15" {
		t.Fatalf("unexpected L610 reset commands: %v", l610.ResetCommands)
	}
}
