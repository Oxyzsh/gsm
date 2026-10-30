package modem

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go-pess/platform/user/pess/gsm/internal/at"
	"go-pess/platform/user/pess/gsm/internal/pdu"
)

func (c *controller) SendVoice(ctx context.Context, number, message string) (StatusResult, error) {
	if dropped := pdu.DroppedNonBMP(message); dropped > 0 {
		c.logger.Warn("voice text contains non-BMP characters; dropping them during UCS2 conversion", "count", dropped)
	}
	phone, err := pdu.NormalizeVoicePhone(number)
	if err != nil {
		return c.currentResult(StatusOperationFailed, "voice number must be exactly 11 digits"), &ValidationError{Field: "phone_number", Reason: err.Error()}
	}
	profile := c.profile()
	if !profile.SupportsVoice || strings.TrimSpace(profile.TTSStyle) == "" {
		return c.currentResult(StatusOperationFailed, "module does not support voice notification"), ErrVoiceUnsupported
	}
	// 两套方言的报文格式不同：移远要 UCS2 十六进制串，广和通直接吃明文。
	hexText := ""
	plainText := ""
	switch profile.TTSStyle {
	case TTSStyleGTTS:
		plainText, err = sanitizeTTSText(message)
		if err != nil {
			return c.currentResult(StatusOperationFailed, "invalid voice text"), &ValidationError{Field: "message", Reason: err.Error()}
		}
	default:
		hexText, err = pdu.EncodeTTS(message)
		if err != nil {
			return c.currentResult(StatusOperationFailed, "invalid voice text"), &ValidationError{Field: "message", Reason: err.Error()}
		}
	}
	if err := c.acquire(ctx); err != nil {
		return c.currentResult(StatusOperationFailed, "request canceled"), err
	}
	defer c.release()

	status, err := c.statusLocked(ctx)
	if err != nil {
		return c.currentResult(StatusOperationFailed, "status check canceled"), err
	}
	if status.Status != StatusOK {
		return status, nil
	}
	operator := c.operatorLocked(ctx)
	if err := voiceOperatorError(profile, operator); err != nil {
		return c.currentResult(StatusOperationFailed, err.Error()), err
	}
	busy, err := c.checkBusyLocked(ctx)
	if err != nil {
		if isContextError(err) {
			return c.currentResult(StatusOperationFailed, "request canceled"), err
		}
		return c.currentResult(StatusOperationFailed, err.Error()), nil
	}
	if busy {
		return c.currentResult(StatusBusy, StatusBusy.Detail()), nil
	}

	defer c.hangup(ctx)
	if _, err := c.command(ctx, "ATD"+phone+";", 30*time.Second); err != nil {
		if isContextError(err) {
			return c.currentResult(StatusOperationFailed, "request canceled"), err
		}
		return c.currentResult(StatusOperationFailed, voiceFailureDetail("dial", err)), nil
	}

	if err := c.waitForActiveCall(ctx, phone); err != nil {
		if isContextError(err) {
			return c.currentResult(StatusOperationFailed, "request canceled"), err
		}
		return c.currentResult(StatusOperationFailed, err.Error()), nil
	}

	ttsCommand := voiceTTSCommand(profile.TTSStyle, hexText, plainText)
	if _, err := c.command(ctx, ttsCommand, 10*time.Second); err != nil {
		if isContextError(err) {
			return c.currentResult(StatusOperationFailed, "request canceled"), err
		}
		return c.currentResult(StatusOperationFailed, voiceFailureDetail("TTS start", err)), nil
	}
	if err := c.waitForTTS(ctx, profile.TTSStyle); err != nil {
		if isContextError(err) {
			return c.currentResult(StatusOperationFailed, "request canceled"), err
		}
		return c.currentResult(StatusOperationFailed, err.Error()), nil
	}
	return c.currentResult(StatusOK, StatusOK.Detail()), nil
}

func (c *controller) waitForActiveCall(ctx context.Context, targetPhone string) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		if !time.Now().Before(deadline) {
			return errors.New("voice call was not answered within 15s")
		}
		if err := wait(ctx, c.cfg.PollInterval); err != nil {
			return err
		}
		reply, err := c.command(ctx, "AT+CLCC", 3*time.Second)
		if err != nil {
			if isContextError(err) {
				return err
			}
			if time.Now().After(deadline) {
				return errors.New("voice call was not answered within 15s")
			}
			c.logger.Warn("AT+CLCC polling failed; continuing", "error", err)
			continue
		}
		if hasActiveCallForNumber(reply.Lines, targetPhone) {
			return nil
		}
	}
}

func (c *controller) waitForTTS(ctx context.Context, style string) error {
	deadline := time.Now().Add(40 * time.Second)
	for {
		if !time.Now().Before(deadline) {
			return errors.New("TTS playback did not finish within 40s")
		}
		if err := wait(ctx, c.cfg.PollInterval); err != nil {
			return err
		}
		query := "AT+QWTTS?"
		pattern := qwttsPattern
		if style == TTSStyleGTTS {
			query = "AT+GTTS?"
			pattern = gttsIdlePattern
		}
		reply, err := c.command(ctx, query, 3*time.Second)
		if err != nil {
			if isContextError(err) {
				return err
			}
			if time.Now().After(deadline) {
				return errors.New("TTS playback did not finish within 40s")
			}
			c.logger.Warn("TTS progress polling failed; continuing", "query", query, "error", err)
			continue
		}
		if pattern.MatchString(strings.Join(reply.Lines, "\n")) {
			return nil
		}
	}
}

func (c *controller) hangup(ctx context.Context) {
	hangupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := c.transport.Do(hangupCtx, "ATH"); err != nil {
		c.logger.Warn("failed to hang up voice call", "error", err)
	}
}

// voiceTTSCommand 按方言生成播报命令。
// 移远吃 UCS2 十六进制串；广和通吃明文（参数含义待 Fibocom 手册确认）。
func voiceTTSCommand(style, hexText, plainText string) string {
	if style == TTSStyleGTTS {
		return fmt.Sprintf("AT+GTTS=1,\"%s\",0", plainText)
	}
	return fmt.Sprintf("AT+QWTTS=1,1,1,\"%s\"", hexText)
}

// sanitizeTTSText 把播报文本清成单行、去掉会破坏 AT 命令的引号和换行。
// 广和通的 AT+GTTS 直接吃明文，长度上限与移远一致（480 个 BMP 字符）。
func sanitizeTTSText(message string) (string, error) {
	replacer := strings.NewReplacer("\u000d", " ", "\u000a", " ", "\u0022", " ", "\u005c", " ")
	cleaned := strings.Join(strings.Fields(replacer.Replace(message)), " ")
	if cleaned == "" {
		return "", fmt.Errorf("%w: voice text is empty", pdu.ErrTextTooLong)
	}
	if runes := []rune(cleaned); len(runes) > pdu.MaxTTSCharacters {
		return "", fmt.Errorf("%w: %d BMP characters exceeds %d", pdu.ErrTextTooLong, len(runes), pdu.MaxTTSCharacters)
	}
	return cleaned, nil
}

// hasActiveCallForNumber 只接受目标号码的外呼已接通记录。
// 部分模块会同时返回多条号码为空的 state=0 伪通话，不能把任意 state=0
// 当作目标电话已经接通。
func hasActiveCallForNumber(lines []string, targetPhone string) bool {
	targetPhone = normalizeCLCCNumber(targetPhone)
	if targetPhone == "" {
		return false
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(trimmed), "+CLCC:") {
			continue
		}
		colon := strings.IndexByte(trimmed, ':')
		if colon < 0 || colon+1 >= len(trimmed) {
			continue
		}
		fields := strings.Split(strings.TrimSpace(trimmed[colon+1:]), ",")
		if len(fields) < 6 {
			continue
		}
		direction, dirErr := strconv.Atoi(strings.TrimSpace(fields[1]))
		state, stateErr := strconv.Atoi(strings.TrimSpace(fields[2]))
		if dirErr != nil || stateErr != nil || direction != 0 || state != 0 {
			continue
		}
		if normalizeCLCCNumber(fields[5]) == targetPhone {
			return true
		}
	}
	return false
}

func normalizeCLCCNumber(value string) string {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	var digits strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	number := digits.String()
	if len(number) == 13 && strings.HasPrefix(number, "86") {
		return number[2:]
	}
	return number
}

func voiceFailureDetail(operation string, err error) string {
	var terminal *at.TerminalError
	if errors.As(err, &terminal) {
		return fmt.Sprintf("%s failed: %s", operation, terminal.Error())
	}
	return fmt.Sprintf("%s failed: %v", operation, err)
}

type operatorClass int

const (
	operatorUnknown operatorClass = iota
	operatorChinaMobile
	operatorChinaTelecom
	operatorChinaUnicom
	operatorOther
)

func (c *controller) operatorLocked(ctx context.Context) operatorClass {
	reply, err := c.command(ctx, "AT+COPS?", 3*time.Second)
	if err != nil {
		if ctx.Err() == nil {
			c.logger.Warn("AT+COPS? failed; carrier gating will use unknown operator", "error", err)
		}
		return operatorUnknown
	}
	text := strings.ToUpper(reply.Raw + "\n" + strings.Join(reply.Lines, "\n"))
	switch {
	case strings.Contains(text, "CHN-CMCC"), strings.Contains(text, "CHINA MOBILE"), strings.Contains(text, "CMCC"):
		return operatorChinaMobile
	case strings.Contains(text, "CHN-CT"), strings.Contains(text, "CHINA TELECOM"), strings.Contains(text, "CTCC"):
		return operatorChinaTelecom
	case strings.Contains(text, "CHN-UNICOM"), strings.Contains(text, "CHINA UNICOM"), strings.Contains(text, "CUCC"):
		return operatorChinaUnicom
	case strings.Contains(text, "CHN-"), strings.Contains(text, ","):
		return operatorOther
	default:
		return operatorUnknown
	}
}

func voiceOperatorError(profile moduleProfile, operator operatorClass) error {
	if operator == operatorUnknown {
		return nil
	}
	if profile.Type == ModuleEM05CE && operator != operatorChinaMobile {
		return fmt.Errorf("%w: EM05-CE supports China Mobile only", ErrVoiceUnsupported)
	}
	if profile.Type == ModuleEM05CN && profile.Firmware == FirmwareA03 && operator == operatorChinaUnicom {
		return fmt.Errorf("%w: EM05-CN A03 does not support China Unicom VoLTE voice", ErrVoiceUnsupported)
	}
	return nil
}
