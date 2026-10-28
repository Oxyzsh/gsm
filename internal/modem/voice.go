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
	hexText, err := pdu.EncodeTTS(message)
	if err != nil {
		return c.currentResult(StatusOperationFailed, "invalid voice text"), &ValidationError{Field: "message", Reason: err.Error()}
	}
	profile := c.profile()
	if !profile.SupportsVoice {
		return c.currentResult(StatusOperationFailed, "module does not support QWTTS voice"), ErrVoiceUnsupported
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

	if err := c.waitForActiveCall(ctx); err != nil {
		if isContextError(err) {
			return c.currentResult(StatusOperationFailed, "request canceled"), err
		}
		return c.currentResult(StatusOperationFailed, err.Error()), nil
	}

	if _, err := c.command(ctx, fmt.Sprintf(`AT+QWTTS=1,1,1,"%s"`, hexText), 10*time.Second); err != nil {
		if isContextError(err) {
			return c.currentResult(StatusOperationFailed, "request canceled"), err
		}
		return c.currentResult(StatusOperationFailed, voiceFailureDetail("TTS start", err)), nil
	}
	if err := c.waitForTTS(ctx); err != nil {
		if isContextError(err) {
			return c.currentResult(StatusOperationFailed, "request canceled"), err
		}
		return c.currentResult(StatusOperationFailed, err.Error()), nil
	}
	return c.currentResult(StatusOK, StatusOK.Detail()), nil
}

func (c *controller) waitForActiveCall(ctx context.Context) error {
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
		if hasActiveCall(reply.Lines) {
			return nil
		}
	}
}

func (c *controller) waitForTTS(ctx context.Context) error {
	deadline := time.Now().Add(40 * time.Second)
	for {
		if !time.Now().Before(deadline) {
			return errors.New("TTS playback did not finish within 40s")
		}
		if err := wait(ctx, c.cfg.PollInterval); err != nil {
			return err
		}
		reply, err := c.command(ctx, "AT+QWTTS?", 3*time.Second)
		if err != nil {
			if isContextError(err) {
				return err
			}
			if time.Now().After(deadline) {
				return errors.New("TTS playback did not finish within 40s")
			}
			c.logger.Warn("AT+QWTTS? polling failed; continuing", "error", err)
			continue
		}
		if qwttsPattern.MatchString(strings.Join(reply.Lines, "\n")) {
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

func hasActiveCall(lines []string) bool {
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
		if len(fields) < 3 {
			continue
		}
		state, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err == nil && state == 0 {
			return true
		}
	}
	return false
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
