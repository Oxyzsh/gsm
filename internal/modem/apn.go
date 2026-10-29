package modem

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// apnAuto 是 module.apn 的自动模式：按 SIM 的 IMSI 判断运营商，再用该运营商的常用 APN。
const apnAuto = "auto"

// imsiPattern 从 AT+CIMI 的应答里取纯数字行。
var imsiPattern = regexp.MustCompile(`^\d{6,17}$`)

// operatorFromIMSI 用 IMSI 前 5 位（MCC+MNC）判断运营商。
//
// 为什么用 IMSI 而不是 AT+COPS?：COPS 要注册成功才有值，而"卡在但注册不上"
// （例如把这家的卡插到定制模块里）正是最需要判断运营商的时候。
func operatorFromIMSI(imsi string) operatorClass {
	imsi = strings.TrimSpace(imsi)
	if len(imsi) < 5 {
		return operatorUnknown
	}
	switch imsi[:5] {
	case "46000", "46002", "46004", "46007", "46008":
		return operatorChinaMobile
	case "46001", "46006", "46009":
		return operatorChinaUnicom
	case "46003", "46005", "46011":
		return operatorChinaTelecom
	default:
		return operatorOther
	}
}

// apnForOperator 返回运营商的常用 APN；未知运营商返回空串（保持现状，不乱下发）。
//
// 物联网/定向卡可能用非标 APN，所以 module.apn 填具体值时优先，不做覆盖。
func apnForOperator(operator operatorClass) string {
	switch operator {
	case operatorChinaMobile:
		return "cmnet"
	case operatorChinaUnicom:
		return "3gnet"
	case operatorChinaTelecom:
		return "ctnet"
	default:
		return ""
	}
}

// imsiFromReply 取 AT+CIMI 应答里的 IMSI；读不到卡时模块返回 ERROR，这里返回空串。
func imsiFromReply(lines []string) string {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if imsiPattern.MatchString(line) {
			return line
		}
	}
	return ""
}

// ensureAPNLocked 在 SIM 就绪后把 APN 下发给模块。
//
// apn=auto：读 AT+CIMI 判运营商，取该运营商的常用 APN；读不到卡或判不出运营商就保持模块原状。
// apn=具体值：以配置为准（现场用非标 APN 的物联网卡走这条）。
// 同一个 APN 只下发一次，避免每次状态轮询都重复 AT+CGDCONT。
func (c *controller) ensureAPNLocked(ctx context.Context) {
	apn := strings.TrimSpace(c.cfg.APN)
	if apn == "" {
		return
	}
	if strings.EqualFold(apn, apnAuto) {
		if c.apnResolved == "" {
			reply, err := c.command(ctx, "AT+CIMI", 3*time.Second)
			if err != nil {
				if ctx.Err() == nil {
					c.logger.Warn("AT+CIMI failed; APN stays as module default", "error", err)
				}
				return
			}
			imsi := imsiFromReply(reply.Lines)
			if imsi == "" {
				c.logger.Warn("IMSI not available; APN stays as module default", "raw", reply.Raw)
				return
			}
			c.apnResolved = apnForOperator(operatorFromIMSI(imsi))
			if c.apnResolved == "" {
				c.logger.Warn("no default APN for this carrier; APN stays as module default", "imsi_prefix", imsi[:5])
				return
			}
			c.logger.Info("APN resolved from SIM", "imsi_prefix", imsi[:5], "apn", c.apnResolved)
		}
		apn = c.apnResolved
	}
	if apn == "" || apn == c.apnApplied {
		return
	}
	command := fmt.Sprintf(`AT+CGDCONT=1,"IP","%s"`, apn)
	reply, err := c.command(ctx, command, 3*time.Second)
	if err != nil {
		if ctx.Err() == nil {
			c.logger.Warn("apply APN failed", "apn", apn, "error", err)
		}
		return
	}
	if !reply.OK() {
		c.logger.Warn("apply APN rejected by module", "apn", apn, "raw", reply.Raw)
		return
	}
	c.apnApplied = apn
	c.logger.Info("APN applied", "apn", apn)
}

// resetAPNLocked 返回复位后要写进模块的 APN；auto 模式在还没读到卡时返回空（不写 "auto"）。
func (c *controller) resetAPNLocked() string {
	apn := strings.TrimSpace(c.cfg.APN)
	if strings.EqualFold(apn, apnAuto) {
		return c.apnResolved
	}
	return apn
}
