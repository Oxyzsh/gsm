# 4G 模块兼容矩阵

`gsmd` 通过模块能力表支持以下设备，避免把厂商专用 AT 命令发给不兼容的模块。

## 配置值

```yaml
module:
  type: auto
  version_file: /etc/mmdc/4g_version
```

| `module.type` | 说明 |
| --- | --- |
| `auto` | 优先读取版本文件，随后通过 `ATI`、`AT+CGMM`、`AT+CGMR` 自动识别 |
| `em05-ce` | 移远 EM05-CE，中国移动 |
| `em05-cn` | 移远 EM05-CN，固件版本自动识别 |
| `em05-cn-a03` | 固定使用 A03 能力，联通 VoLTE 语音不支持 |
| `em05-cn-a04` | 固定使用 A04 能力，支持移动、电信、联通 VoLTE |
| `fibocom` / `fibocom-l610` | 广和通 L610 |

## 能力矩阵

| 能力 | EM05-CE | EM05-CN A03 | EM05-CN A04 | Fibocom L610 |
| --- | --- | --- | --- | --- |
| `AT` / `CPIN` / `CSQ` 状态检测 | 支持 | 支持 | 支持 | 支持 |
| PDU 短信，DCS=`08` | 支持 | 支持 | 支持 | 支持 |
| 运营商 | 移动 | 移动、电信；联通 VoLTE 不支持 | 移动、电信、联通 | 三网通 |
| 数据重拨 | `AT+cgact=0,1` / `1,1` | 同左 | 同左 | `AT+GTRNDIS=0,1` / `1,1` |
| 数据模式配置 | `AT+qnetdevctl=1,1,1` | 同左 | 同左 | 不发送 Quectel 命令 |
| 复位 | `AT^reset`，失败回退 `AT+CFUN=1,1` | 同左 | 同左 | `AT+CFUN=15`，失败回退 `AT+CFUN=1,1` |
| 电信短信存储配置 | `AT+QCFG="ltectcc/smsstorage",1` | 同左 | 同左 | 不发送 Quectel 命令 |
| `AT+QWTTS` 文本语音 | 支持 | 支持非联通 VoLTE | 支持 | 不支持，返回 `FailedPrecondition` |

## 串口设备

AT 串口编号随模块系列、USB 模式和主机枚举顺序变化，不能写死为同一种：

- 现有 EM05 现场样例使用 `/dev/ttyUSB2`。
- 广和通 L610 调试样例使用 `/dev/ttyUSB5`。

部署前应通过 `ls -l /dev/serial/by-id`、`udevadm info` 或 `ATI` 确认实际 AT 口。

## 自动识别

1. 读取 `module.version_file`。
2. 匹配 `EM05CE`、`EM05CN...A03/A04`、`FIBOCOM` 或 `L610`。
3. 无法从文件识别时，在串口可用后查询：

```text
ATI
AT+CGMM
AT+CGMR
```

`GetStatusResponse.hw_type` 返回 `em05-ce`、`em05-cn` 或 `fibocom`，新增的 `firmware` 字段返回 `A03`、`A04`、`L610` 或 `unknown`。

## 运营商语音门禁

- EM05-CE 在检测到非中国移动运营商时，`SendVoice` 返回 `FailedPrecondition`。
- EM05-CN A03 在检测到中国联通时，`SendVoice` 返回 `FailedPrecondition`。
- EM05-CN A04 不做运营商语音限制。
- L610 不支持当前 QWTTS 文本播放入口。

## 来源依据

- `../4G手册/README.txt`
- `../4G手册/4G调试相关/4G调试相关说明.txt`
- `../广和通L610/广和通模块调试测试.docx`
- `../广和通L610/Fibocom_MTC_拨号指南_Linux_V1.3.pdf`
