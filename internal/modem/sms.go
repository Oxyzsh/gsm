package modem

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go-pess/platform/user/pess/gsm/internal/at"
	"go-pess/platform/user/pess/gsm/internal/pdu"
)

func (c *controller) SendSMS(ctx context.Context, number, message string) (SendResult, error) {
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
		length, err := pdu.CMGSLength(pduHex)
		if err != nil {
			return SendResult{Status: StatusOperationFailed, Detail: err.Error()}, nil
		}
		c.logger.Debug("submitting SMS segment", "segment", index+1, "cmgs_length", length, "pdu_bytes", len(pduHex)/2)
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
			return SendResult{Status: StatusOperationFailed, Detail: fmt.Sprintf("segment %d/%d failed: %s", index+1, len(pdus), reply.Final())}, nil
		}
	}
	return SendResult{Status: StatusOK, Detail: StatusOK.Detail(), Segments: len(pdus)}, nil
}

func (c *controller) smsFailure(ctx context.Context, segment int, operation string, err error) (SendResult, error) {
	if isContextError(err) {
		return SendResult{Status: StatusOperationFailed, Detail: "request canceled; sent segments are not rolled back"}, err
	}
	var terminal *at.TerminalError
	if errors.As(err, &terminal) {
		return SendResult{Status: StatusOperationFailed, Detail: fmt.Sprintf("segment %d %s failed: %s", segment, operation, terminal.Error())}, nil
	}
	return SendResult{Status: StatusOperationFailed, Detail: fmt.Sprintf("segment %d %s failed: %v", segment, operation, err)}, nil
}
