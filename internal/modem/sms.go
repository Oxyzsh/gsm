package modem

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gsm/internal/at"
	"gsm/internal/pdu"
)

// SendSMS 发送短信，并把每次尝试的结果写进日志（号码打码）。
//
// 之前失败只把原因放在返回值里，现场只能看到页面一句"操作失败"，journal 里查不到任何线索。
func (c *controller) SendSMS(ctx context.Context, number, message string) (SendResult, error) {
	result, err := c.sendSMS(ctx, number, message)
	masked := maskNumber(number)
	switch {
	case err != nil:
		c.logger.Warn("SMS send aborted", "number", masked, "error", err)
	case result.Status != StatusOK:
		c.logger.Warn("SMS send failed", "number", masked, "status", int(result.Status), "detail", result.Detail)
	default:
		c.logger.Info("SMS sent", "number", masked, "segments", result.Segments)
	}
	return result, err
}

// segmentPause 是长短信相邻两段之间的间隔（测试里置 0）。
var segmentPause = 300 * time.Millisecond

// maskNumber 只保留前 3 位和后 4 位，避免把完整手机号写进日志。
func maskNumber(number string) string {
	trimmed := strings.TrimSpace(number)
	if len(trimmed) <= 7 {
		return trimmed + "***"
	}
	return trimmed[:3] + "****" + trimmed[len(trimmed)-4:]
}

func (c *controller) sendSMS(ctx context.Context, number, message string) (SendResult, error) {
	if dropped := pdu.DroppedNonBMP(message); dropped > 0 {
		c.logger.Warn("SMS contains non-BMP characters; dropping them during UCS2 conversion", "count", dropped)
	}
	pdus, err := pdu.EncodeSMS(number, message)
	if err != nil {
		return SendResult{}, &ValidationError{Field: "SMS request", Reason: err.Error()}
	}
	if err := c.acquire(ctx); err != nil {
		return SendResult{Status: StatusOperationFailed, Detail: "request canceled"}, err
	}
	defer c.release()

	status, err := c.statusLocked(ctx)
	if err != nil {
		return SendResult{Status: StatusOperationFailed, Detail: "status check canceled"}, err
	}
	if status.Status != StatusOK {
		return SendResult{Status: status.Status, Detail: status.Detail}, nil
	}
	busy, err := c.checkBusyLocked(ctx)
	if err != nil {
		if isContextError(err) {
			return SendResult{Status: StatusOperationFailed, Detail: "request canceled"}, err
		}
		return SendResult{Status: StatusOperationFailed, Detail: err.Error()}, nil
	}
	if busy {
		return SendResult{Status: StatusBusy, Detail: StatusBusy.Detail()}, nil
	}

	if _, err := c.command(ctx, "AT", 3*time.Second); err != nil {
		return c.smsFailure(ctx, 0, "AT", err)
	}
	if _, err := c.command(ctx, "AT+CMGF=0", 3*time.Second); err != nil {
		return c.smsFailure(ctx, 0, "AT+CMGF=0", err)
	}
	if _, err := c.command(ctx, "AT+CMEE=2", 3*time.Second); err != nil {
		if isContextError(err) {
			return c.smsFailure(ctx, 0, "AT+CMEE=2", err)
		}
		c.logger.Warn("AT+CMEE=2 failed; continuing", "error", err)
	}
	profile := c.profile()
	if cops, err := c.command(ctx, "AT+COPS?", 3*time.Second); err == nil && profile.SMSStorageCommand != "" && cops.Contains("CHN-CT") {
		if _, cfgErr := c.command(ctx, profile.SMSStorageCommand, 3*time.Second); cfgErr != nil {
			if isContextError(cfgErr) {
				return c.smsFailure(ctx, 0, "telecom SMS storage configuration", cfgErr)
			}
			c.logger.Warn("telecom SMS storage configuration failed; continuing", "error", cfgErr)
		}
	} else if isContextError(err) {
		return c.smsFailure(ctx, 0, "AT+COPS?", err)
	}

	for index, pduHex := range pdus {
		// 长短信是 N 条独立短信，逐条提交。段间留一点间隔：运营商对同一号码
		// 有速率限制，16 段连着打容易被限速或被模块缓冲挤掉。
		if index > 0 && segmentPause > 0 {
			select {
			case <-time.After(segmentPause):
			case <-ctx.Done():
				return c.smsFailure(ctx, index+1, "segment pacing", ctx.Err())
			}
		}
		length, err := pdu.CMGSLength(pduHex)
		if err != nil {
			return SendResult{Status: StatusOperationFailed, Detail: err.Error()}, nil
		}
		c.logger.Debug("submitting SMS segment", "segment", index+1, "total", len(pdus), "cmgs_length", length, "pdu_bytes", len(pduHex)/2)
		promptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = c.transport.WaitPrompt(promptCtx, "AT+CMGS="+strconv.Itoa(length))
		cancel()
		if err != nil {
			return c.smsFailure(ctx, index+1, "AT+CMGS", err)
		}

		data := make([]byte, 0, len(pduHex)+1)
		data = append(data, []byte(pduHex)...)
		data = append(data, 0x1a)
		submitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		reply, err := c.transport.SendRaw(submitCtx, data)
		cancel()
		if err != nil || !reply.OK() {
			if err != nil {
				return c.smsFailure(ctx, index+1, "SMS PDU submission", err)
			}
			return SendResult{Status: StatusOperationFailed, Detail: fmt.Sprintf(
				"segment %d/%d failed: %s（前 %d 段已提交，不做重试）", index+1, len(pdus), reply.Final(), index)}, nil
		}
	}
	return SendResult{Status: StatusOK, Detail: StatusOK.Detail(), Segments: len(pdus)}, nil
}

func (c *controller) smsFailure(ctx context.Context, segment int, operation string, err error) (SendResult, error) {
	if isContextError(err) {
		return SendResult{Status: StatusOperationFailed, Detail: "request canceled; sent segments are not rolled back"}, err
	}
	submitted := segment - 1
	if submitted < 0 {
		submitted = 0
	}
	var terminal *at.TerminalError
	if errors.As(err, &terminal) {
		return SendResult{Status: StatusOperationFailed, Detail: fmt.Sprintf(
			"segment %d %s failed: %s（前 %d 段已提交，不做重试）", segment, operation, terminal.Error(), submitted)}, nil
	}
	return SendResult{Status: StatusOperationFailed, Detail: fmt.Sprintf(
		"segment %d %s failed: %v（前 %d 段已提交，不做重试）", segment, operation, err, submitted)}, nil
}
