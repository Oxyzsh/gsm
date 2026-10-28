# GSM（4G 模块）Go 版本开发文档

| 项目 | 内容 |
| --- | --- |
| 文档版本 | 1.0 |
| 日期 | 2026-10-28 |
| 状态 | 设计基线（评审后作为实现依据） |
| 适用代码 | 当前目录下 C 实现（`src/`、`files/`、`Makefile`） |
| 目标产物 | Go 单进程服务 `gsmd`，gRPC 接口，通用 Linux + systemd |

本文档描述将现有 OpenWrt C 实现迁移为 Go 版本的完整设计，包括现状分析、架构、模块职责、接口定义、配置、关键流程、测试、部署与实施计划。文档中的“必须”“不得”为验收要求；“建议”为可选优化。

---

## 1. 背景与目标

### 1.1 背景

`gsm` 是 PESS 设备侧负责 4G 模块管理的服务，当前为 OpenWrt 上的 C 程序 `gsmd`，通过 ubus 对外提供能力，通过 UCI 读取配置，使用 procd 托管。PESS 设备侧正在整体迁移到 Go，gsm 是其中最后一个待迁移模块。迁移后运行环境从 OpenWrt 变为通用 Linux 服务器，由 systemd 托管。

现有能力：

- 4G 模块状态检测（禁用、无硬件、无 SIM、无信号、正常）
- 模块使能/禁用、重拨、复位
- 短信发送（PDU 模式、UCS2 中文、长短信分片）
- 语音外呼与 TTS 文本播放（仅移远 EM05-CE/CN）
- 周期性保活检测与有限自愈

### 1.2 目标

1. 用 Go 重写 `gsmd`，行为上覆盖上述现有能力。
2. 对外仅提供 gRPC 接口，不再保留 ubus、HTTP JSON-RPC 兼容路径。
3. 运行于通用 Linux，配置文件为 YAML，由 systemd 托管。
4. 串口常驻、并发安全，消除“每条 AT 命令开关一次串口”的实现。
5. 关键逻辑（PDU 编码、AT 应答解析、保活阈值）可单元测试。
6. 移除高风险行为：不再自动重启整机。

### 1.3 非目标

- 不兼容 OpenWrt 的 ubus/UCI/procd。
- 不实现 ping 连通性检测（C 版本中该逻辑已注释停用）。
- 不实现短信接收、短信回执（delivery report）、彩信、数据拨号管理。
- 不实现多 4G 模块并发管理，一个进程只管理一个模块。
- 不实现配置热加载；配置变更后重启服务。
- 不提供 TLS/mTLS；默认只监听回环地址。
- 不管理模块供电/GPIO 复位。

### 1.4 术语

| 术语 | 说明 |
| --- | --- |
| AT 命令 | 调制解调器控制命令，以 `\r\n` 结束，以 `OK`/`ERROR` 结束应答 |
| URC | 非请求结果码（Unsolicited Result Code），模块主动上报，如 `RING` |
| PDU | 短信协议数据单元，见 3GPP TS 23.040 |
| UCS2 | 16 位定长 Unicode 编码，中文短信使用 |
| SMSC | 短信中心地址 |
| UDH | 用户数据头，长短信分片使用 |
| DCS | 数据编码方案，`0x08` 表示 UCS2 |
| CSQ | 信号质量，`AT+CSQ` 返回 0–31，99 表示未知 |
| CPAS | 模块活动状态，`AT+CPAS` 返回 0 表示空闲 |
| CLCC | 当前通话列表，`AT+CLCC` |
| ECM | 以太网控制模型，模块以网卡方式提供数据连接 |
| 业务操作锁 | Go 侧互斥锁，保证一段多命令业务流程不被插入 |

---

## 2. 现状分析（C 版本）

### 2.1 代码结构

| 文件 | 职责 |
| --- | --- |
| `src/gsm_main.c` | 入口、参数解析、coredump、初始化/退出、`system()`/`popen()` 封装 |
| `src/gsm_cfg.c` | 通过 `uci get` 子进程读取 `/etc/config/gsm` |
| `src/gsm_drv.c` | 串口打开/配置、AT 命令执行、状态检测、使能/重拨/复位、短信/语音流程 |
| `src/gsm_keepalive.c` | 周期状态检测与重拨/复位/重启策略 |
| `src/gsm_encode.c` | UTF-8 → UCS2、号码 BCD 编码、PDU 头与分片、TTS 文本编码 |
| `src/gsm_ubus.c` | ubus 对象 `gsm`，方法 `debug`/`enable`/`sendmsg`/`sendvoice` |
| `files/gsm.conf` | UCI 配置样例 |
| `files/gsm.init` | procd 启动脚本 |
| `Makefile`、`src/CMakeLists.txt` | OpenWrt 打包与 CMake 构建 |

### 2.2 现有外部接口

ubus 对象路径：`gsm`。

| 方法 | 参数 | 返回 | 行为 |
| --- | --- | --- | --- |
| `debug` | `value: int32` | ubus 状态码 | 运行时开关调试日志 |
| `enable` | `value: int32` | ubus 状态码 | 使能/禁用模块，状态变化时先复位再拨号 |
| `sendmsg` | `number: string`，`msg: string` | `{result: string}` | 发送短信，返回状态字符串 |
| `sendvoice` | `number: string`，`msg: string` | `{result: string}` | 语音外呼并播放 TTS 文本 |

### 2.3 现有配置

文件：`/etc/config/gsm`，通过 UCI 读取。

| 节 | 选项 | 默认 | 说明 |
| --- | --- | --- | --- |
| `global.device` | `devname` | 无（必填） | 串口设备，如 `/dev/ttyUSB2` |
| | `baudrate` | 115200 | 波特率 |
| | `apn` | 无（必填） | APN，如 `cmnet`、`ctnet` |
| `global.keepalive` | `main_server` | `www.baidu.com` | ping 主服务器（功能已停用） |
| | `backup_server` | 无 | ping 备服务器（功能已停用） |
| | `check_interval` | 300 | 检测间隔（秒），但代码未生效 |

### 2.4 关键流程

**初始化**：解析参数 → `gsm_cfg_load` → `gsm_drv_init`（读取 `/etc/mmdc/4g_version` 判定模块类型）→ `gsm_ka_init`（5 秒后首次检测）→ `gsm_ubus_init`。

**状态检测**：`AT` 失败 → `NO_HW`；`AT+CPIN?` 不含 `READY` → `NO_CARD`；`AT+CSQ` 的 RSSI < 16 或 99 → `NO_SIG`；否则 `OK`。`DIAL_FAIL` 与 `BUSY` 不会由状态检测返回。

**保活**：每 20 秒执行一次状态检测；`status < OK` 时只记录日志并返回；`DIAL_FAIL`/`BUSY` 时执行“重拨 6 次 → 复位 → 重启整机”的阈值逻辑。**该自愈分支实际不可达**：状态检测从不返回 `DIAL_FAIL`/`BUSY`，且这两个值在枚举中排在 `OK` 之前，即使返回也会被 `status < OK` 分支提前捕获；ping 检测也已被注释。

**短信**：号码归一化为“86 + 11 位”（要求总长 13）；UTF-8 转 UCS2；按 140 字节单片、134 字节分片封装 PDU；`AT+CMGF=0`、`AT+CMEE=2`；对每个分片执行 `AT+CMGS=<长度>`，收到 `>` 后发送 PDU + `0x1A`，等待 `OK`。

**语音**：`ATD<号码>;` → 每秒 `AT+CLCC` 等待通话接通（最长 15 秒）→ `AT+QWTTS=1,1,1,"<UCS2 十六进制>"` 播放 → 轮询 `AT+QWTTS?` 等待播放完成（最长 40 秒）→ `ATH` 挂断。

### 2.5 问题与风险清单

| 级别 | 问题 | 影响 |
| --- | --- | --- |
| 高 | 每条 AT 命令都 `open`/`close` 串口 | 性能差；缓冲区残留导致应答串扰；无法处理 URC |
| 高 | PDU 目的地址长度固定为 `0D`、类型固定 `91` | 仅支持 13 位号码，其他号码生成非法 PDU |
| 高 | 分片数量上限 16 但未校验总长度 | 超长文本会写越界 |
| 高 | 保活自愈分支不可达；间隔配置未生效 | 自愈名存实亡，检测节奏与配置不符 |
| 高 | 失败后自动 `reboot` 整机 | 通用服务器上可能造成大范围中断 |
| 中 | 解析 `AT+CPAS` 时按固定下标 `reply[9]` 取值 | 回显/空白变化即误判 |
| 中 | `AT+CLCC` 用子串 `0,0,0` 判接通 | 字段位置变化易误判 |
| 中 | 语音文本拼进 1024 字节命令缓冲，超长会被截断 | 播放内容不完整 |
| 中 | 大量阻塞 `sleep`，无取消机制 | 调用方无法超时；关停不优雅 |
| 中 | 全局可变状态、非线程安全 | 无法安全并发调用 |
| 中 | 依赖 `uci`、`cat`、`ping`、`reboot` 子进程 | 脱离 OpenWrt 后不可用 |
| 低 | 无任何自动化测试 | 回归风险高 |
| 低 | `system()` 前临时改 `SIGCHLD` | 与事件循环耦合，易出隐患 |
| 低 | coredump 由进程自身设置 | 应由 systemd/内核策略管理 |

---

## 3. 总体设计

### 3.1 架构分层

```text
                    gRPC 调用方（本机）
                           │
                  ┌────────▼────────┐
                  │  internal/api   │  gRPC 服务实现、状态/错误映射
                  └────────┬────────┘
                           │ modem.Controller
                  ┌────────▼────────┐        ┌───────────────────┐
                  │ internal/modem  │───────▶│   internal/pdu    │
                  │ 状态、短信、语音 │        │ 号码/PDU/编码（纯函数）│
                  └────────┬────────┘        └───────────────────┘
                           │ at.Transport
                  ┌────────▼────────┐
                  │   internal/at   │  串口管理、行解析、AT 执行器、重连
                  └────────┬────────┘
                           │ serial.Port
                     /dev/ttyUSBx

     internal/keepalive ──▶ modem.Controller（周期检测与阈值动作）
     internal/config    ──▶ cmd/gsmd（YAML 加载、校验）
```

依赖方向严格自上而下：`api` 只依赖 `modem` 接口，`modem` 只依赖 `at` 接口与 `pdu` 纯函数，`at` 只依赖串口抽象。`keepalive` 与 `api` 并列，均依赖 `modem`。禁止反向依赖。

### 3.2 进程与并发模型

单进程，主要 goroutine：

| Goroutine | 职责 | 退出方式 |
| --- | --- | --- |
| main | 解析参数、装配依赖、启动各组件、等待信号 | 收到 SIGINT/SIGTERM |
| gRPC server | 监听、处理 RPC | 根 context 取消后 `GracefulStop`，5 秒后 `Stop` |
| keepalive | 周期检测与自愈 | 根 context 取消 |
| serial reader | 阻塞读串口、按行/提示符分帧 | 串口关闭或 context 取消 |
| reconnect supervisor | 串口断开后的退避重连 | 根 context 取消 |

并发约束：

- 串口是半双工设备，**所有 AT 命令由同一执行器串行发送**。
- 一段多命令业务流程（状态检查、短信、语音、使能/复位）持有**业务操作锁**（`opMu`，可被 context 取消）。其他业务调用在锁上排队；客户端应设置合理的 gRPC deadline。
- 后台保活与前台 RPC 共用同一把业务锁。保活可能等待一次进行中的语音呼叫（最长约 100 秒），这不会影响前台调用，只是延后本轮检测。
- 串口读协程只读不写；写操作全部经执行器，避免读写竞争。

### 3.3 目录结构

```text
gsm/
├── cmd/
│   └── gsmd/
│       └── main.go                 # 入口：flag、装配、信号、优雅退出
├── internal/
│   ├── config/
│   │   ├── config.go               # YAML 加载、默认值、校验、Duration 类型
│   │   └── config_test.go
│   ├── at/
│   │   ├── session.go              # 串口会话、重连监督
│   │   ├── framer.go               # 行/提示符分帧、终止符识别
│   │   ├── executor.go             # Do / WaitPrompt / SendRaw
│   │   ├── errors.go
│   │   └── *_test.go
│   ├── modem/
│   │   ├── modem.go                # 状态、使能、重拨、复位、忙检测
│   │   ├── sms.go                  # 短信发送流程
│   │   ├── voice.go                # 语音外呼与 TTS 流程
│   │   └── *_test.go
│   ├── pdu/
│   │   ├── phone.go                # 号码归一化
│   │   ├── encode.go               # UCS2、PDU 头、分片
│   │   └── *_test.go
│   ├── keepalive/
│   │   ├── keepalive.go            # 调度、阈值、日志
│   │   └── keepalive_test.go
│   └── api/
│       ├── server.go               # gRPC 服务实现
│       └── server_test.go
├── api/
│   ├── proto/pess/gsm/v1/gsm.proto # 接口定义（权威）
│   └── gen/pess/gsm/v1/            # protoc 生成代码（随仓库保存）
├── configs/
│   └── gsm.yaml                    # 示例配置
├── deploy/
│   └── gsmd.service                # systemd unit
├── docs/
│   └── gsm-go-dev.md               # 本文档
├── Makefile
├── go.mod
└── go.sum
```

模块路径暂定 `go-pess/platform/user/pess/gsm`，与当前 GOPATH 布局（`d:/go/src`）一致。若迁移到公司代码仓库，只替换 `go.mod` 的 `module` 与 proto 的 `go_package`，其余 import 由工具重写。

### 3.4 依赖

| 依赖 | 用途 | 说明 |
| --- | --- | --- |
| `google.golang.org/grpc` | gRPC 服务与客户端 | 必选；示例使用 `grpc.NewClient`（≥ 1.63） |
| `google.golang.org/protobuf` | 生成代码运行时 | 必选 |
| `gopkg.in/yaml.v3` | 配置解析 | 支持严格字段校验 |
| `go.bug.st/serial` | 串口打开与参数配置 | 跨平台、维护活跃 |
| `golang.org/x/text` | UTF-16/UCS2 编码 | 替代 iconv |
| `google.golang.org/grpc/test/bufconn` | RPC 单测 | 测试专用 |

不引入 ORM、日志框架、DI 框架；日志使用标准库 `log/slog`。

Go 版本要求：1.22 及以上（使用 `log/slog`、`context.WithoutCancel`）。

### 3.5 启动与退出

启动顺序（任一步失败则退出，退出码非 0）：

1. 解析命令行参数（`-config`、`-debug`、`-version`）。
2. 加载并校验 YAML 配置；`-debug` 覆盖日志级别。
3. 初始化日志（级别、格式、输出到 stderr，由 journald 收集）。
4. 创建串口会话并启动读协程/重连监督（此时不要求设备立即存在）。
5. 创建 modem 控制器；读取模块版本文件判定类型（失败默认 EM05-CE，告警）。
6. 启动 gRPC server；若监听地址非回环，记录 WARN。
7. 启动 keepalive。
8. 等待 SIGINT/SIGTERM。

退出顺序：

1. 取消根 context，停止 keepalive。
2. `GracefulStop` gRPC（5 秒超时后强制 `Stop`）。
3. 关闭串口会话，等待读协程退出。
4. 记录退出日志，进程返回 0。

---

## 4. 配置设计

### 4.1 文件位置与格式

- 默认路径：`/etc/gsm/gsm.yaml`，可用 `-config` 覆盖。
- YAML 1.2，UTF-8。
- 严格模式：未知字段直接报错（`yaml.Decoder.KnownFields(true)`），避免拼写错误被静默忽略。
- 时长字段使用 Go duration 字符串（如 `300s`、`10s`），通过自定义 `Duration` 类型解析。

### 4.2 字段定义

| 字段 | 类型 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- | --- |
| `serial.device` | string | 是 | — | 串口设备路径 |
| `serial.baud_rate` | int | 否 | `115200` | 波特率，必须大于 0 |
| `module.type` | string | 否 | `auto` | `auto`/`em05-ce`/`em05-cn`/`em05-cn-a03`/`em05-cn-a04`/`fibocom`/`fibocom-l610` |
| `module.version_file` | string | 否 | `/etc/mmdc/4g_version` | `auto` 模式下读取的版本文件 |
| `apn` | string | 是 | — | 移动 `cmnet`，电信 `ctnet`；仅复位流程使用 |
| `keepalive.enabled` | bool | 否 | `true` | 是否启动保活 |
| `keepalive.check_interval` | duration | 否 | `300s` | 正常/不可自动恢复状态的检测间隔 |
| `keepalive.retry_interval` | duration | 否 | `10s` | 执行重拨/复位后的下次检测间隔 |
| `keepalive.redial_threshold` | int | 否 | `5` | 重拨计数 **大于** 该值时进入复位步骤 |
| `keepalive.reset_threshold` | int | 否 | `10` | 复位计数 **大于** 该值时停止自动动作 |
| `grpc.listen` | string | 否 | `127.0.0.1:50051` | gRPC 监听地址，`host:port` |
| `log.level` | string | 否 | `info` | `debug`/`info`/`warn`/`error` |
| `log.format` | string | 否 | `text` | `text`/`json` |

阈值语义与 C 一致：C 使用 `redialCnt > 5`，因此默认配置在重拨计数达到 6 时执行复位；`resetCnt > 10` 时原版执行 reboot，Go 版改为停止自动动作（见 10.4）。

### 4.3 示例

```yaml
serial:
  device: /dev/ttyUSB2
  baud_rate: 115200

module:
  type: auto
  version_file: /etc/mmdc/4g_version

apn: cmnet

keepalive:
  enabled: true
  check_interval: 60s
  retry_interval: 10s
  redial_threshold: 5
  reset_threshold: 10

grpc:
  listen: 127.0.0.1:50051

log:
  level: info
  format: text
```

### 4.4 校验规则

加载后逐项校验，失败时记录具体字段名并以退出码 2 退出：

- `serial.device` 非空，建议为绝对路径。
- `serial.baud_rate` 在 110–4000000 之间；具体支持范围由串口库决定。
- `module.type` 属于枚举值；`module.version_file` 在 `auto` 模式下非空。
- `apn` 非空。
- 两个 interval ≥ 1s；两个 threshold ≥ 1。
- `grpc.listen` 可被 `net.SplitHostPort` 解析；端口 1–65535。
- `log.level`、`log.format` 属于枚举值。

### 4.5 与 UCI 配置的映射

| UCI | YAML | 处理 |
| --- | --- | --- |
| `global.device.devname` | `serial.device` | 直接映射 |
| `global.device.baudrate` | `serial.baud_rate` | 直接映射 |
| `global.device.apn` | `apn` | 直接映射 |
| `global.keepalive.check_interval` | `keepalive.check_interval` | 秒数转为 duration |
| `global.keepalive.main_server` | 删除 | 对应 ping 检测在 C 中已停用 |
| `global.keepalive.backup_server` | 删除 | 同上 |
| 无 | `module.*` | 新增，替代硬编码的版本文件路径与类型 |
| 无 | `grpc.listen` | 新增 |
| 无 | `log.*` | 新增 |

### 4.6 Duration 类型实现

```go
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
    var raw string
    if err := value.Decode(&raw); err != nil {
        return err
    }
    parsed, err := time.ParseDuration(raw)
    if err != nil {
        return fmt.Errorf("invalid duration %q: %w", raw, err)
    }
    *d = Duration(parsed)
    return nil
}
```

---

## 5. 串口与 AT 协议层

### 5.1 职责边界

`internal/at` 只负责“把一条命令可靠地发出去，并把这一次命令的应答收回来”。它不理解短信、语音、状态语义，不持有业务状态。业务层通过 `at.Transport` 接口使用它，测试时可替换为脚本化的假实现。

### 5.2 串口连接与重连

- 使用 `go.bug.st/serial` 打开配置中的设备，参数固定为 8 数据位、无校验、1 停止位、无流控，波特率取配置。
- 串口**常驻打开**，进程生命周期内不随命令开关。
- 读协程阻塞读取；写入仅由执行器在持有互斥锁时进行。
- 读错误、写错误或设备消失时：关闭端口，标记为 down，按 1s → 2s → 4s … 30s 的退避重连；重连成功后清零退避。
- 端口 down 期间，`Do`/`SendRaw` 立即返回 `ErrPortDown`，业务层映射为 `NO_HW`。
- 每次重连成功后，先尽力发送 `ATE0`（关闭回显），再标记为可用。回显仍被容忍：与所发命令相同的行会被忽略。
- `Reopen(ctx)` 供业务层强制重开（保活复位步骤使用）。

单实例约束：C 版本使用 `O_EXCL` 打开串口；Go 串口库不提供同等语义。部署时通过 systemd 保证只有一个 `gsmd` 实例，不额外实现文件锁。

### 5.3 分帧与应答识别

读协程把字节流转换为事件：

| 事件 | 触发条件 | 说明 |
| --- | --- | --- |
| Line | 读到 `\n` | 去掉行尾 `\r`；空行保留为空字符串 |
| Prompt | 读到 `>` | `AT+CMGS` 的输入提示不带换行，必须单独识别 |
| Overflow | 当前行超过 4096 字节 | 丢弃该行并记录 WARN，防止无换行输出耗尽内存 |

终止符识别（行首匹配）：

- `OK`
- `ERROR`
- `+CME ERROR:`
- `+CMS ERROR:`

`Do` 的收集规则：持续收集 Line，直到出现终止符或超时。终止符之前的行全部保留在 `Reply.Lines` 中。超时后到达的行属于迟到应答，在**下一条命令发送前**被非阻塞清空，避免串扰。

URC 处理：本设计不使用模块主动上报，语音与状态均采用轮询（与 C 一致）。不属于当前命令应答的行记录 DEBUG 日志后丢弃。该限制在 17 章列为已知风险。

### 5.4 执行器接口

```go
type Reply struct {
    Lines []string // 已去掉行尾 \r，按到达顺序
    Raw   string   // 原始应答拼接，便于调试日志
}

func (r Reply) Final() string          // "OK" / "ERROR" / "+CME ERROR" / "+CMS ERROR" / ""
func (r Reply) OK() bool               // Final() == "OK"
func (r Reply) Contains(s string) bool // 任一行包含子串

type Session struct{ /* ... */ }

func Open(cfg Config) (*Session, error)
func (s *Session) Run(ctx context.Context) error       // 读协程 + 重连监督，阻塞
func (s *Session) Do(ctx context.Context, cmd string) (Reply, error)
func (s *Session) WaitPrompt(ctx context.Context, cmd string) error
func (s *Session) SendRaw(ctx context.Context, data []byte) (Reply, error)
func (s *Session) Reopen(ctx context.Context) error
func (s *Session) Close() error

// Transport 是业务层依赖的接口，测试时可用脚本化假实现替换。
type Transport interface {
    Do(ctx context.Context, cmd string) (Reply, error)
    WaitPrompt(ctx context.Context, cmd string) error
    SendRaw(ctx context.Context, data []byte) (Reply, error)
}
```

约定：

- `cmd` 不含结尾的 `\r\n`；`Do` 自动追加。
- `Do` 收到终止符即返回：终止符为 `OK` 时 `err == nil`；为 `ERROR`/`+CME ERROR`/`+CMS ERROR` 时返回 `*TerminalError`，同时 `Reply` 仍然有效，调用方可读取原始错误行。
- `WaitPrompt` 发送命令后等待 `>` 事件；若先收到 `ERROR`/`+CME ERROR`/`+CMS ERROR`，返回 `*TerminalError`。
- `SendRaw` 原样写入字节（短信 PDU + `0x1A`），不追加换行，然后收集到终止符，错误语义与 `Do` 相同。
- 所有方法都接受 context；超时与取消由调用方控制，执行器内部只提供默认超时常量。

### 5.5 超时与取消

| 阶段 | 默认超时 | 位置 |
| --- | --- | --- |
| 普通 AT 命令 | 3s | modem 层为每次调用设置 |
| `AT+CMGS` 等待 `>` | 5s | sms 层 |
| PDU 提交等待终止符 | 30s | sms 层 |
| 语音拨号等待 `OK` | 30s | voice 层 |
| 等待接听（轮询 `AT+CLCC`） | 15s | voice 层 |
| `AT+QWTTS` 启动等待 `OK` | 10s | voice 层 |
| 播放完成（轮询 `AT+QWTTS?`） | 40s | voice 层 |
| 挂断 `ATH` | 5s | voice 层 |
| 复位后等待模块恢复 | 60s | modem 层 |

取消语义：

- 前台 RPC 的 context 被取消时，流程尽快返回。
- 语音流程无论成功、失败还是取消，都必须尝试挂断；挂断使用 `context.WithoutCancel(ctx)` 派生的、带 5 秒超时的独立 context，避免使用已取消的 ctx。
- 短信流程不可回滚：取消或中途失败时，已发送的分片无法撤回，必须在文档与日志中明确。

### 5.6 错误分类

| 错误 | 含义 | 业务映射 |
| --- | --- | --- |
| `ErrTimeout` | 命令在超时内未收到终止符 | `OPERATION_FAILED`（发送流程）或状态检测失败 |
| `ErrPortDown` | 串口未打开/已断开 | `NO_HW` |
| `ErrClosed` | 会话已关闭（进程退出中） | `OPERATION_FAILED` / RPC `Unavailable` |
| `*TerminalError` | 收到 `ERROR`/`+CME ERROR`/`+CMS ERROR` | `OPERATION_FAILED`，`detail` 携带原始错误行 |

### 5.7 串口参数

| 参数 | 值 |
| --- | --- |
| 数据位 | 8 |
| 校验 | 无 |
| 停止位 | 1 |
| 流控 | 无 |
| 原始模式 | 由串口库保证（关闭回显、行模式、信号处理） |

C 版本调用 `cfmakeraw` 并逐项设置 termios；Go 使用串口库的 `serial.Mode{BaudRate, DataBits: 8, Parity: serial.NoParity, StopBits: serial.OneStopBit}`，效果等价。

---

## 6. 调制解调器业务层

### 6.1 状态模型

Go 内部状态与 C 保持相同顺序，便于沿用 `status < StatusOK` 的判定：

```go
type Status int

const (
    StatusDisabled        Status = iota // 被主动禁用
    StatusNoHW                          // 硬件不存在/串口不可用
    StatusNoCard                        // SIM 卡未插入
    StatusNoSignal                      // 无信号
    StatusDialFail                      // 拨号失败（当前状态检测不产生）
    StatusBusy                          // 模块忙（发送流程产生）
    StatusOK                            // 正常
    StatusOperationFailed               // Go 新增：短信/语音 AT 流程失败
)
```

`StatusOperationFailed` 放在 `StatusOK` 之后，不参与 `status < StatusOK` 的保活判定。

### 6.2 状态检测

`Status(ctx)` 获取业务锁后依次执行：

1. 若运行时 `enabled == false`，直接返回 `DISABLED`。
2. 发送 `AT`，3 秒超时；失败返回 `NO_HW`。
3. 发送 `AT+CPIN?`；应答不包含 `READY` 返回 `NO_CARD`。
4. 发送 `AT+CSQ`；用正则 `\+CSQ:\s*(\d+)\s*,\s*(\d+)` 解析 RSSI/BER：
   - 解析失败：记录 WARN，继续。
   - RSSI < 16 或 RSSI == 99：返回 `NO_SIGNAL`。RSSI=16 在 EM05-CN A04 上实测仍可发送短信。
   - 其他：记录 RSSI/BER。
5. 返回 `OK`。

与 C 的差异：不再用 `sscanf` 的宽松匹配，改为显式正则；RSSI 阈值按现场 A04 实测调整为 `<16` 视为无信号；RSSI=16 可继续短信流程。

`StatusResult` 同时携带：状态、RSSI、BER、模块类型、检测时间、人可读说明（英文），供 gRPC 返回。

### 6.3 使能与禁用

`SetEnabled(ctx, enabled)`：

- 若与当前运行时状态相同：直接返回当前状态，不发送任何 AT 命令。
- 若不同：
  1. 更新运行时状态。
  2. 执行 `Reset`（见 6.4）。
  3. 使能时执行模块能力表对应的上/下行重拨；禁用时执行对应的断开命令。
- 运行时状态不持久化；进程重启后默认 `enabled = true`（与 C 一致）。

### 6.4 复位、重拨与重新初始化

**Redial**（对应 C `gsm_drv_redial`）：

1. 按模块能力表断开数据：EM05 使用 `AT+cgact=0,1`，L610 使用 `AT+GTRNDIS=0,1`。
2. 按模块能力表重新拨号：EM05 使用 `AT+cgact=1,1`，L610 使用 `AT+GTRNDIS=1,1`。
3. 均尽力执行，失败记录 WARN。

**Reset**（对应 C `gsm_drv_reset`，由 `SetEnabled` 调用）：

1. 按模块能力表尝试复位命令：EM05 先 `AT^reset`、回退 `AT+CFUN=1,1`；L610 先 `AT+CFUN=15`、回退 `AT+CFUN=1,1`。
2. 若上一步成功：关闭并等待串口恢复。模块重启可能导致 tty 消失/重新枚举，以 1 秒间隔探测设备可打开且 `AT` 返回 `OK`，最长 60 秒；若超时，返回错误，`SetEnabled` 返回 `OPERATION_FAILED`。若 `AT^reset` 失败（固件不支持或返回 ERROR）：记录 WARN，跳过等待，继续后续步骤。
3. `ATE0`（关闭回显）。
4. `AT+cgdcont=1,"IP","<apn>"`。
5. 若模块类型为 EM05-CE 或 EM05-CN：`AT+qnetdevctl=1,1,1`。
6. `AT+ipr=<baud>`。
7. 以上 3–6 为尽力执行，失败仅告警，不改变返回值。

注意：不同厂商复位语义不同。EM05 与 L610 均按能力表逐条尝试并记录原始应答，只有成功命令才触发串口恢复等待。

**Reinitialize**（对应 C 保活中的“复位”：`gsm_drv_fini` + `gsm_drv_init`）：

- 重新读取模块版本文件判定类型。
- 强制关闭并重开串口会话。
- 不发送 `AT^reset`，不修改模块配置。与 C 语义一致。
- 由保活阈值触发（见 10.3）。

### 6.5 忙检测

`AT+CPAS` 应答格式：`+CPAS: <status>`，0 表示 ready，3 表示振铃，4 表示通话中或保持。

实现：正则解析整数；非 0 即忙。C 使用固定下标 `reply[9]`，在存在回显或空白时误判，Go 改为显式解析。

发送短信/语音前执行两次忙检测，间隔 1 秒（与 C 一致，第一次用于清理残留，第二次容错）。仍忙则返回 `BUSY`。

### 6.6 业务操作锁与接口

```go
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
    Detail    string
    RSSI      int
    BER       int
    HWType    string
    CheckedAt time.Time
}

type SendResult struct {
    Status   Status
    Detail   string
    Segments int
}
```

- 每个公开方法内部获取业务锁（可被 ctx 取消）；`Redial`/`Reinitialize` 同样加锁。
- 串口执行器自身也有互斥锁；业务锁用于保证多命令流程的原子性，两者职责不同，不可合并。
- gRPC 层只依赖 `Controller` 接口；测试用假实现替换。
- 业务层不感知 gRPC，也不感知保活调度。

### 6.7 模块类型判定

- 配置 `module.type` 非 `auto` 时采用指定类型；`em05-cn-a03`/`em05-cn-a04` 可固定固件能力。
- `auto` 时读取 `module.version_file`（默认 `/etc/mmdc/4g_version`），在文件内容中查找：
  - 含 `EM05CE` → `em05-ce`
  - 含 `EM05CN` 及 `A03`/`A04` → `em05-cn`，同时记录固件能力
  - 含 `L610`/`LC610`/`FIBOCOM` → `fibocom`
  - 文件不可读或未命中时不默认成 EM05-CE；串口可用后查询 `ATI`、`AT+CGMM`、`AT+CGMR` 自动识别。
- 模块能力表决定语音、复位、重拨和厂商专用短信配置命令。
- `Reinitialize` 重新执行判定；`GetStatus` 返回 `hw_type` 和 `firmware`。

---

## 7. 短信功能

### 7.1 号码归一化

规则（`internal/pdu.NormalizePhone`）：

1. 去除首尾空白。
2. 去除开头的 `+`。
3. 若以 `0086` 开头，去掉一个 `0`，得到 `86...`。
4. 若以 `086` 开头，去掉一个 `0`，得到 `86...`。
5. 剩余部分必须全部为 ASCII 数字。
6. 若长度为 11，前面补 `86`。
7. 最终必须满足：总长 13 且以 `86` 开头，否则返回错误。

第 3、4 步按顺序执行（不是二选一），因此 `0086...` 会连续去掉两个 `0`。

示例：

| 输入 | 输出 | 结果 |
| --- | --- | --- |
| `13612345678` | `8613612345678` | 成功 |
| `+8613612345678` | `8613612345678` | 成功 |
| `08613612345678` | 处理后 `8613612345678` | 成功 |
| `008613612345678` | 处理后 `8613612345678` | 成功 |
| `8613612345678` | `8613612345678` | 成功 |
| `1361234567` | — | 失败（10 位） |
| `+1 555 0100` | — | 失败（含空格、长度不符） |

语音号码规则不同：按 C 行为，要求原始输入恰好 11 位且全为数字，不做国家码补齐，直接用于 `ATD<号码>;`。`+86` 形式在语音接口中返回 `InvalidArgument`。

### 7.2 PDU 编码格式

采用与 C 相同的 UCS2 PDU（不实现 7-bit GSM 编码，英文也按 UCS2 发送）：

| 字段 | 值 | 说明 |
| --- | --- | --- |
| SMSC | `00` | 使用模块默认短信中心，长度字节为 0 |
| First Octet | `01`（单片）/ `41`（分片） | `0x01` 不带有效期；`0x41` 另含 UDH。EM05-CN A04 已验证该格式。 |
| TP-MR | `00` | 消息引用值，发送时填 0 |
| TP-DA 长度 | 按实际数字位数计算 | 13 位号码为 `0D`；不再硬编码 |
| TP-DA 类型 | `91` | 国际号码 |
| TP-DA 数字 | BCD 交换，奇数位补 `F` | 例：`8613612345678` → `683116325476F8` |
| TP-PID | `00` | |
| TP-DCS | `08` | UCS2 |
| TP-VP | `AA` | 相对有效期 |
| TP-UDL | UD 字节数 | 分片时包含 6 字节 UDH |
| TP-UD | UCS2 大端字节，转大写十六进制 | 分片时前置 UDH |

UDH 格式（6 字节）：`05 00 03 01 <总片数> <当前片序号>`。参考号固定为 `01`（与 C 一致，所有分片共享）。

### 7.3 UTF-8 转 UCS2

- 使用 `golang.org/x/text/encoding/unicode`，按大端 UTF-16 编码。
- 仅接受 BMP 字符（≤ `U+FFFF`）。非 BMP 字符（如 emoji）被丢弃并记录 WARN，对应 C 的 `UCS-2//IGNORE`。含代理对的 UTF-16 输出不用于 PDU，避免生成非法 UCS2。
- 若转换后为空（例如文本仅含 emoji），返回 `InvalidArgument`。
- 每字符固定 2 字节。

### 7.4 分片规则

设 UCS2 数据总字节数为 `N`：

- `N ≤ 140`：单片，TH-UD 最多 140 字节（70 个 BMP 字符）。
- `N > 140`：分片，每片数据载荷 `140 − 6 = 134` 字节（67 个字符）；总片数 `ceil(N / 134)`。
- 总片数上限 16 片，即最多 `16 × 134 = 2144` 字节、1072 个 BMP 字符；超过返回 `InvalidArgument`，**不得**生成越界 PDU。
- 每片 UDL = 6 + 本片数据字节数；末片可少于 134 字节。

边界示例：

| 字符数 | 片数 | 说明 |
| --- | --- | --- |
| 70 | 1 | 140 字节，单片上限 |
| 71 | 2 | 第一片 67 字符，第二片 4 字符 |
| 1072 | 16 | 允许的最大长度 |
| 1073 | — | `InvalidArgument` |

### 7.5 发送流程

1. 获取业务锁。
2. 归一化号码、编码 PDU（失败 → `InvalidArgument`）。
3. `Status` 检查：若不为 `OK`，返回对应状态（`DISABLED`/`NO_HW`/`NO_CARD`/`NO_SIGNAL`）。
4. 忙检测两次（间隔 1 秒）；忙则 `BUSY`。
5. 发送 `AT`（3 秒），失败 → `OPERATION_FAILED`。
6. 发送 `AT+CMGF=0`，失败 → `OPERATION_FAILED`。
7. 发送 `AT+CMEE=2`（尽力）。
8. 发送 `AT+COPS?`；若应答包含 `CHN-CT`，尽力发送 `AT+QCFG="ltectcc/smsstorage",1`（电信卡短信存储配置）。
9. 对每个分片：
   - `WaitPrompt(ctx, "AT+CMGS=<PDU 长度>")`，超时 5 秒；长度 = （PDU 十六进制字符数 − 2）/ 2，不含 SMSC。
   - 收到 `>` 后 `SendRaw(PDU 字节 + 0x1A)`，等待终止符，超时 30 秒。
   - 终止符为 `OK` 视为该片成功；`ERROR`/`+CMS ERROR`/`+CME ERROR`/超时视为失败，中止后续分片。
10. 全部成功返回 `OK`，`segments` 为片数；否则返回 `OPERATION_FAILED`，`detail` 包含失败片序号与原始错误。

时序：

```text
modem                     module
  │  AT+CMGF=0               │
  │─────────────────────────▶│
  │◀──────────────── OK ─────│
  │  AT+CMGS=18              │
  │─────────────────────────▶│
  │◀─────────────────────────│  ">"
  │  0011...597D + 0x1A      │
  │─────────────────────────▶│
  │◀─────────────── OK ──────│
```

失败语义：短信发送不是事务性的；第 1 片成功、第 2 片失败时，第 1 片已发出，接口返回失败但不回滚。调用方需自行决定是否重发。

### 7.6 错误与限制

| 场景 | 返回 |
| --- | --- |
| 号码为空/格式错误 | gRPC `InvalidArgument` |
| 文本为空或转换后为空 | gRPC `InvalidArgument` |
| 文本超过 1072 个 BMP 字符 | gRPC `InvalidArgument` |
| 模块禁用/无硬件/无卡/无信号 | 响应 `GsmStatus` 对应值 |
| 模块忙 | 响应 `BUSY` |
| AT 命令失败、超时、模块返回 CME/CMS 错误 | 响应 `OPERATION_FAILED`，`detail` 带原因 |
| 客户端取消/超时 | gRPC `DeadlineExceeded`/`Canceled`；已发送分片不撤回 |

### 7.7 测试向量

固定号码 `8613612345678`：

| 文本 | 期望 PDU | `AT+CMGS` 长度 |
| --- | --- | --- |
| `你好` | `0001000D91683116325476F80008044F60597D` | `18` |
| `OK` | `0001000D91683116325476F8000804004F004B` | `18` |

这两组作为 `internal/pdu` 的黄金用例。分片用例断言：71 个 `啊`（U+554A）生成 2 片，第一片 UDL 为 `0x8C`（140），第二片为 `0x0E`（8 字节数据 + 6 字节 UDH）；UDH 分别为 `050003010201`、`050003010202`。

---

## 8. 语音功能

### 8.1 前置条件

- 模块类型为 EM05-CE 或 EM05-CN。C 定义了 `gsm_check_support_voice()` 但未调用；Go 版在流程开始前检查，不支持时返回 gRPC `FailedPrecondition`。
- 号码恰好 11 位数字（见 7.1）。
- 文本 UTF-8，按 UCS2 大端编码为十六进制大写字符串；最多 480 个 BMP 字符（`AT+QWTTS` 整条命令需控制在 1024 字节内，C 的超长文本会被截断）。
- 模块状态为 `OK`，否则返回对应状态。

### 8.2 流程

1. 获取业务锁，检查使能/状态/忙（两次，间隔 1 秒）。
2. 校验号码与文本；编码 TTS 十六进制串。
3. `ATD<号码>;`，30 秒内需收到 `OK`；否则失败。
4. 每秒执行 `AT+CLCC`，解析通话状态；当存在 `stat == 0`（active）的通话时进入播放；最长等待 15 秒。单次命令失败记录 WARN 并继续轮询。
5. `AT+QWTTS=1,1,1,"<十六进制>"`，10 秒内需收到 `OK`；否则失败。
6. 每秒执行 `AT+QWTTS?`，应答出现 `+QWTTS: 0` 表示播放完成；最长轮询 40 秒。单次命令失败记录 WARN 并继续轮询。
7. 无论成功/失败/取消，最后执行 `ATH` 挂断（独立 5 秒 context），失败仅告警。

时序：

```text
modem                         module
  │  ATD13612345678;             │
  │─────────────────────────────▶│
  │◀────────────────────── OK ───│
  │  AT+CLCC（每 1s，最多 15s）    │
  │─────────────────────────────▶│
  │◀── +CLCC: 1,0,0,0,... ───────│
  │  AT+QWTTS=1,1,1,"<hex>"       │
  │─────────────────────────────▶│
  │◀────────────────────── OK ───│
  │  AT+QWTTS?（每 1s，最多 40s） │
  │─────────────────────────────▶│
  │◀──────────── +QWTTS: 0 ──────│
  │  ATH                          │
  │─────────────────────────────▶│
```

### 8.3 应答解析

`AT+CLCC` 行格式：`+CLCC: <id>,<dir>,<stat>,<mode>,<mpty>,<number>,<type>[,<alpha>]`。

- 仅解析以 `+CLCC:` 开头的行，按逗号分割，取第 3 个字段（下标 2）为 `stat`。
- `stat == 0` 表示通话激活，可播放。
- 解析失败的行忽略并记录 DEBUG。
- 不再使用 C 的子串 `"0,0,0"` 判定，避免字段变化误判。

`AT+QWTTS?` 应答中 `+QWTTS: 0` 表示空闲/播放完成；非 0 继续等待。

### 8.4 取消与资源清理

- 等待接听、播放轮询均以 1 秒为粒度检查 context；取消后立即进入挂断。
- 挂断使用 `context.WithoutCancel(ctx)` + 5 秒超时，确保客户端取消后仍能挂断。
- 返回时保留最初的错误原因（超时、取消、AT 失败），挂断失败只记录日志，不覆盖主错误。

### 8.5 限制

- 不实现来电、短信振铃等 URC 处理。
- 不实现通话录音、DTMF。
- 播放进度只能通过 `AT+QWTTS?` 轮询，模块差异需硬件验证。
- 一次只支持一路语音呼叫；业务锁保证串行。

---

## 9. gRPC API

### 9.1 服务定义

权威文件：`api/proto/pess/gsm/v1/gsm.proto`。

```proto
syntax = "proto3";

package pess.gsm.v1;

import "google/protobuf/timestamp.proto";

option go_package = "go-pess/platform/user/pess/gsm/api/gen/pess/gsm/v1;gsmv1";

// GsmStatus 表示 4G 模块的可用状态。
enum GsmStatus {
  GSM_STATUS_UNSPECIFIED = 0;
  GSM_STATUS_OK = 1;
  GSM_STATUS_DISABLED = 2;          // 被主动禁用
  GSM_STATUS_NO_HW = 3;             // 硬件不存在或串口不可用
  GSM_STATUS_NO_CARD = 4;           // SIM 卡未插入
  GSM_STATUS_NO_SIGNAL = 5;         // 无信号
  GSM_STATUS_DIAL_FAIL = 6;         // 拨号失败（当前状态检测不产生，保留）
  GSM_STATUS_BUSY = 7;              // 模块忙
  GSM_STATUS_OPERATION_FAILED = 8;  // 短信/语音 AT 流程失败
}

message GetStatusRequest {}

message GetStatusResponse {
  GsmStatus status = 1;
  bool enabled = 2;
  int32 rssi = 3;          // CSQ 0-31；-1 表示未知
  int32 ber = 4;           // 误码率 0-7，99 未知；-1 表示未获取
  string hw_type = 5;      // "em05-ce" | "em05-cn" | "fibocom" | "unknown"
  string firmware = 8;     // "A03" | "A04" | "L610" | "unknown"
  string detail = 6;       // 人可读说明（英文）
  google.protobuf.Timestamp checked_at = 7;
}

message SetEnabledRequest {
  bool enabled = 1;
}

message SetEnabledResponse {
  GsmStatus status = 1;
  string detail = 2;
}

message SendSMSRequest {
  string phone_number = 1; // 支持 11 位、86 开头 13 位、+86、086、0086
  string message = 2;      // UTF-8，按 UCS2 编码
}

message SendSMSResponse {
  GsmStatus status = 1;
  string detail = 2;
  int32 segments = 3;      // 分片数；失败时为 0
}

message SendVoiceRequest {
  string phone_number = 1; // 必须为 11 位数字
  string message = 2;      // UTF-8，QWTTS 播放文本，最多 480 个 BMP 字符
}

message SendVoiceResponse {
  GsmStatus status = 1;
  string detail = 2;
}

message SetLogLevelRequest {
  enum Level {
    LEVEL_UNSPECIFIED = 0;
    LEVEL_DEBUG = 1;
    LEVEL_INFO = 2;
    LEVEL_WARN = 3;
    LEVEL_ERROR = 4;
  }
  Level level = 1;
}

message SetLogLevelResponse {}

service GsmService {
  rpc GetStatus(GetStatusRequest) returns (GetStatusResponse);
  rpc SetEnabled(SetEnabledRequest) returns (SetEnabledResponse);
  rpc SendSMS(SendSMSRequest) returns (SendSMSResponse);
  rpc SendVoice(SendVoiceRequest) returns (SendVoiceResponse);
  rpc SetLogLevel(SetLogLevelRequest) returns (SetLogLevelResponse);
}
```

生成代码路径：`api/gen/pess/gsm/v1/gsm.pb.go`、`api/gen/pess/gsm/v1/gsm_grpc.pb.go`，包名 `gsmv1`。生成代码随仓库保存，构建不依赖 `protoc`；`make proto` 用于重新生成。

### 9.2 方法语义

| RPC | 行为 | 成功返回 |
| --- | --- | --- |
| `GetStatus` | 实时执行一次状态检测（`AT`/`CPIN`/`CSQ`），可能等待进行中的业务 | 任意 `GsmStatus` + RSSI/BER/模块类型/检测时间 |
| `SetEnabled` | 状态变化时执行 Reset + 拨号/断拨；状态不变则无操作 | 当前状态 |
| `SendSMS` | 归一化号码、PDU 编码、逐片发送 | `OK` + 分片数 |
| `SendVoice` | 外呼、等待接通、TTS 播放、挂断 | `OK` |
| `SetLogLevel` | 运行时修改日志级别 | 空应答 |

状态型结果（`DISABLED`/`NO_HW`/`NO_CARD`/`NO_SIGNAL`/`BUSY`/`OPERATION_FAILED`）通过响应中的 `status` 返回，RPC 本身成功。这样调用方可以统一按业务状态处理，而不是在 gRPC 错误与状态枚举之间切换。

### 9.3 错误映射

| 场景 | gRPC code | 说明 |
| --- | --- | --- |
| 号码为空/格式错误、文本为空/超长 | `InvalidArgument` | 不进入业务状态 |
| 语音号码非 11 位、语音文本超长 | `InvalidArgument` | |
| 模块类型不支持语音 | `FailedPrecondition` | 仅 EM05-CE/CN 支持 |
| 客户端 deadline/取消 | `DeadlineExceeded` / `Canceled` | 传递 context 错误 |
| 服务正在关闭 | `Unavailable` | 停止接收新请求 |
| PDU 编码内部错误、不可达分支 | `Internal` | 记录 ERROR 日志 |
| 模块状态/AT 流程失败 | RPC OK + `GsmStatus` | 见 9.2 |

`detail` 使用英文短句，如 `sim card not inserted`、`module busy`、`AT+CMGS failed: +CMS ERROR: 500`。

### 9.4 超时、并发与取消

建议客户端 deadline：

| RPC | 建议 deadline | 最坏情况 |
| --- | --- | --- |
| `GetStatus` | 15s | 3 条命令各 3s，加排队时间 |
| `SetEnabled` | 120s | 复位后等待模块恢复最长 60s |
| `SendSMS` | 120s（典型）；16 片极端情况建议 600s | 每片 prompt 5s + 应答 30s，最坏约 560s |
| `SendVoice` | 120s | 约 100s |
| `SetLogLevel` | 5s | 立即返回 |

并发语义：

- 所有业务 RPC 串行执行；并发请求在业务锁上排队。
- 排队时间计入调用方 deadline；超时后请求不会执行或在中途取消。
- 取消语音会尽量挂断；取消短信只停止后续分片，已发分片不撤回。
- `SetLogLevel` 不获取业务锁，可随时执行。

### 9.5 调用示例

服务端注册 gRPC reflection（`reflection.Register`），以便 `grpcurl` 在无本地 proto 文件时调试；仅在回环监听场景建议开启。

`grpcurl`：

```bash
# 状态
grpcurl -plaintext 127.0.0.1:50051 pess.gsm.v1.GsmService/GetStatus

# 发短信
grpcurl -plaintext -d '{"phone_number":"13612345678","message":"测试短信"}' \
  127.0.0.1:50051 pess.gsm.v1.GsmService/SendSMS

# 语音
grpcurl -plaintext -d '{"phone_number":"13612345678","message":"告警测试"}' \
  127.0.0.1:50051 pess.gsm.v1.GsmService/SendVoice
```

Go 客户端：

```go
conn, err := grpc.NewClient("127.0.0.1:50051",
    grpc.WithTransportCredentials(insecure.NewCredentials()))
if err != nil {
    return err
}
defer conn.Close()

client := gsmv1.NewGsmServiceClient(conn)
ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
defer cancel()

resp, err := client.SendSMS(ctx, &gsmv1.SendSMSRequest{
    PhoneNumber: "13612345678",
    Message:     "测试短信",
})
if err != nil {
    return fmt.Errorf("send sms: %w", err)
}
if resp.GetStatus() != gsmv1.GsmStatus_GSM_STATUS_OK {
    return fmt.Errorf("send sms: status=%s detail=%s", resp.GetStatus(), resp.GetDetail())
}
```

### 9.6 接口演进规则

- `pess.gsm.v1` 为稳定版本；新增字段必须使用新编号，不得复用已删除编号。
- 不改变既有枚举值含义；新增状态值放在末尾。
- 破坏性变更（删字段、改语义、改包名）升级为 `pess.gsm.v2`，新旧版本可并行一段时间。
- 生成代码与 proto 必须同时提交，保持可追溯。

---

## 10. 保活与自愈

### 10.1 调度

- 启动后等待 5 秒执行首次检测（与 C 一致）。
- 正常或不可自动恢复状态：等待 `keepalive.check_interval`。
- 执行重拨/复位后：等待 `keepalive.retry_interval`。
- `keepalive.enabled=false` 时不启动该 goroutine。
- 所有等待使用可被根 context 取消的定时器，不使用不可中断的 `sleep`。

### 10.2 检测逻辑

按评审决定，**照抄 C 的阈值逻辑**（仅移除 reboot）：

```text
last = NO_HW
redialCount = 0
resetCount = 0
interval = 5s

loop:
  wait(interval)
  result, err = modem.Status(ctx)
  if err != nil:
      log warn("status check failed: ...")
      interval = retry_interval
      continue

  status = result.Status

  # 0. 硬件不可用/禁用：不执行 AT 动作
  if status < StatusOK:
      if status != last:
          log status-specific warning
      last = status
      interval = check_interval
      continue

  # 1. DIAL_FAIL/BUSY 视为失败
  fail = (status != StatusOK)

  # 2. 按阈值处理
  if fail:
      if resetCount > resetThreshold:
          log once alert("auto recovery exhausted, manual intervention required")
          interval = check_interval
      else if redialCount > redialThreshold:
          redialCount = 0
          resetCount++
          modem.Reinitialize(ctx)
          interval = retry_interval
      else:
          redialCount++
          modem.Redial(ctx)
          interval = retry_interval
  else:
      if last != StatusOK:
          log warn("GSM module recovered to OK")
      redialCount = 0
      resetCount = 0
      interval = check_interval

  last = status
```

状态变化的日志（与 C 对齐）：

- `NO_HW`：`GSM module is not found.`
- `NO_CARD`：`SIM card is not inserted.`
- `NO_SIGNAL`：`GSM signal is too weak.`
- 恢复：`GSM module recovered to OK.`
- 自愈耗尽：`GSM auto recovery exhausted; manual intervention required.`（ERROR）

### 10.3 阈值语义

| 计数器 | 比较 | 默认 | 动作 |
| --- | --- | --- | --- |
| `redialCount` | `> redial_threshold` | 5 | 清零，`resetCount++`，执行 `Reinitialize` |
| `resetCount` | `> reset_threshold` | 10 | 停止自动动作，只监控并记录 ERROR |

计数只在状态恢复为 `OK` 时清零，与 C 一致。状态为 `DISABLED`/`NO_HW`/`NO_CARD`/`NO_SIGNAL` 时不改变计数器。

### 10.4 与 C 的差异及原因

| 项 | C | Go | 原因 |
| --- | --- | --- | --- |
| 检测间隔 | 回调固定 20 秒，配置未生效 | 生效：`check_interval`/`retry_interval` | 修复配置失效 |
| reboot | `resetCount > 10` 时 `reboot` | 停止自动动作并告警 | 评审决定彻底移除整机重启 |
| 超阈值后行为 | 每轮重复 reboot | 只监控，不再动作 | 避免在服务器上反复重启 |
| 失败分支可达性 | 结构性不可达 | **保持同样判定，仍不可达** | 评审决定照抄原逻辑；`DIAL_FAIL`/`BUSY` 被 `status < OK` 分支提前捕获 |

> **重要说明**：该 `fail` 分支是**结构性不可达**的，与 C 完全一致。原因有两层：
> 1. `Status` 只返回 `DISABLED`/`NO_HW`/`NO_CARD`/`NO_SIGNAL`/`OK`，从不返回 `DIAL_FAIL`/`BUSY`。
> 2. 即使返回，`DIAL_FAIL`/`BUSY` 在枚举中排在 `OK` 之前，会先被 `status < StatusOK` 分支捕获并 `continue`，永远走不到 `fail = status != StatusOK`。
>
> 按评审决定，Go 版照抄该逻辑，因此重拨/重新初始化在真实运行中不会触发。测试覆盖状态变化日志、间隔选择与恢复清零；阈值分支因不可达不做行为测试。若未来要启用自愈，必须先调整失败判定（例如把失败状态移到 `OK` 之后，或增加显式的 `isRecoverableFailure` 判断），此事项列入 17 章开放问题。

### 10.5 保活日志

- 每轮 DEBUG：状态、间隔、计数器。
- 状态变化 INFO/WARN：见 10.2。
- 重拨/重新初始化 INFO；动作失败 WARN。
- 自动恢复耗尽 ERROR，每个耗尽周期只记录一次，状态变化时重新记录。

---

## 11. 日志与可观测性

### 11.1 日志方案

- 使用标准库 `log/slog`，输出到 stderr，由 systemd/journald 收集。
- 级别：`debug`/`info`/`warn`/`error`；启动时取配置，`-debug` 覆盖为 `debug`；运行时可经 `SetLogLevel` 修改（使用 `slog.LevelVar`）。
- 格式：`text`（默认，便于终端查看）或 `json`（便于日志系统解析）。
- 固定字段：`component`（`main`/`at`/`modem`/`sms`/`voice`/`keepalive`/`api`）、`op`、`duration_ms`、`status`。

### 11.2 内容约束

- AT 收发明细仅在 DEBUG 级别记录。
- INFO 及以上日志中的手机号做掩码：保留前 3 位与后 4 位，中间替换为 `****`。
- 短信/语音正文仅在 DEBUG 级别记录，且截断到 32 个字符；默认级别不得记录正文。
- gRPC 响应的 `detail` 同样掩码手机号，不返回完整号码或正文。
- 不记录敏感配置；当前配置不含密钥。
- 错误日志必须包含 AT 命令名与模块返回的终止行，便于定位。

### 11.3 可观测性边界

本期不实现 Prometheus 指标、不实现 gRPC health 服务、不实现 systemd watchdog。运维通过 journald 与 `GetStatus` 判断健康状态。如后续需要，另立需求。

---

## 12. 构建、部署与运维

### 12.1 构建

Makefile 目标：

| 目标 | 行为 |
| --- | --- |
| `make build` | `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=..." -o bin/gsmd ./cmd/gsmd` |
| `make test` | `go test -race ./...` |
| `make vet` | `go vet ./...` |
| `make proto` | 用 `protoc` 重新生成 `api/gen` |
| `make clean` | 删除 `bin/` |
| `make install` | 安装二进制、配置样例、systemd unit |

生成 proto：

```bash
protoc -I api/proto \
  --go_out=api/gen --go_opt=paths=source_relative \
  --go-grpc_out=api/gen --go-grpc_opt=paths=source_relative \
  api/proto/pess/gsm/v1/gsm.proto
```

需要 `protoc-gen-go` 与 `protoc-gen-go-grpc`，版本在 README 或 Makefile 中固定。生成代码随仓库保存，常规构建不调用 `protoc`。

版本信息通过 `-ldflags -X` 注入 `main.version`、`main.commit`、`main.date`，`gsmd -version` 输出。

### 12.2 交叉编译

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 make build
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 make build
```

静态二进制，无 CGO 依赖，不依赖 iconv、libubus、uci、libubox。

### 12.3 命令行参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-config <path>` | `/etc/gsm/gsm.yaml` | 配置文件 |
| `-debug` | `false` | 覆盖日志级别为 debug |
| `-version` | `false` | 打印版本后退出 |

退出码：`0` 正常退出；`1` 启动/运行失败；`2` 配置无效。

### 12.4 systemd unit

`deploy/gsmd.service`：

```ini
[Unit]
Description=PESS GSM (4G module) service
After=network.target
Wants=network.target

[Service]
Type=simple
ExecStart=/usr/sbin/gsmd -config /etc/gsm/gsm.yaml
Restart=on-failure
RestartPreventExitStatus=2
RestartSec=3s
User=gsmd
Group=dialout
SupplementaryGroups=dialout
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
RestrictNamespaces=true
LockPersonality=true
MemoryDenyWriteExecute=true
# 设备级 cgroup 限制为可选项：路径规则要求设备在服务启动时已存在。
# 如需启用，取消注释并按实际路径调整：
# DevicePolicy=closed
# DeviceAllow=/dev/ttyUSB2 rw

[Install]
WantedBy=multi-user.target
```

说明：

- `Restart=on-failure`：进程异常退出时重启；正常 SIGTERM 退出不重启。
- `RestartPreventExitStatus=2`：配置无效（退出码 2）时不反复重启，便于运维发现配置错误。
- 设备级 cgroup 限制默认关闭：若设备节点在服务启动时尚未枚举，`DeviceAllow` 的路径规则无法解析，可能导致启动即失败。需要更强隔离时，用 udev 固定设备路径并确认启动顺序后再启用。
- 若模块重新枚举后路径变化（如 `ttyUSB2` → `ttyUSB3`），需调整配置与 unit。

### 12.5 权限与 udev

- 创建系统用户：`useradd -r -s /usr/sbin/nologin -G dialout gsmd`。
- 串口默认属 `dialout` 组，权限 `0660`。
- 如设备节点权限不符，增加 udev 规则（按实际 VID/PID 调整）：

```udev
SUBSYSTEM=="tty", ATTRS{idVendor}=="2c7c", MODE="0660", GROUP="dialout"
```

- 配置文件权限 `0644`；不含敏感信息。

### 12.6 安装步骤

```bash
install -Dm0755 bin/gsmd /usr/sbin/gsmd
install -Dm0644 configs/gsm.yaml /etc/gsm/gsm.yaml
install -Dm0644 deploy/gsmd.service /etc/systemd/system/gsmd.service
systemctl daemon-reload
systemctl enable --now gsmd
systemctl status gsmd
journalctl -u gsmd -f
```

### 12.7 升级与回滚

- 升级：替换二进制 → `systemctl restart gsmd` → 用 `GetStatus` 验证。
- 回滚：保留上一版本二进制（如 `/usr/sbin/gsmd.bak`），替换后重启。
- 配置变更：修改 YAML → 重启服务；服务不做热加载。

### 12.8 故障排查

| 现象 | 排查 |
| --- | --- |
| 启动失败，配置错误 | 检查日志中的字段名；确认 YAML 缩进与 duration 格式 |
| `NO_HW` | 检查 `/dev/ttyUSB*` 是否存在、权限、`lsusb`、journal 中的串口错误 |
| `NO_CARD` | 检查 SIM 卡座与 `AT+CPIN?` 的原始应答（DEBUG 日志） |
| `NO_SIGNAL` | 检查天线、`AT+CSQ` 原始值；注意 RSSI < 16 才判为无信号 |
| `BUSY` | 检查是否有进行中的呼叫；`AT+CPAS` 原始应答 |
| 短信失败 | 查看 `+CMS ERROR` 行；确认 APN/短信中心配置；尝试 `AT+CMGF=0` 手动验证 |
| 语音失败 | 确认模块类型为 EM05-CE/CN、SIM 支持语音、号码可接通 |
| gRPC 连接拒绝 | 检查 `grpc.listen`、服务状态、端口占用 |
| 串口反复重连 | 检查 USB 供电/线缆；DEBUG 日志中的重连退避与错误 |

---

## 13. 测试策略

### 13.1 单元测试矩阵

| 包 | 用例 |
| --- | --- |
| `pdu` | 号码归一化（11/13/`+86`/`086`/`0086`/非法字符/长度错误）；UCS2 转换；黄金 PDU 向量；70/71 字符边界；16 片上限；超长拒绝；非 BMP 丢弃；文本仅 emoji 报错；DA 长度计算 |
| `config` | 默认值；必填缺失；未知字段报错；duration 解析；枚举校验；阈值/端口范围 |
| `at` | 假串口驱动：`OK`/`ERROR`/`+CME`/`+CMS`；超时；`>` 提示；回显忽略；迟到应答清理；超长行；断线重连；`Reopen` |
| `modem` | 状态检测各分支（AT 失败、CPIN 无 READY、CSQ 0/16/17/99/解析失败）；忙检测；SMS 成功/单片/多片/中途失败；语音接通/超时/无应答/取消挂断；使能与复位 |
| `keepalive` | 状态变化日志；间隔选择；恢复清零；`enabled=false` 时不启动；失败阈值分支不可达（见 10.4） |
| `api` | bufconn 下的 RPC；InvalidArgument；FailedPrecondition；状态映射；并发排队；取消传播 |

### 13.2 测试替身

- **假串口**：实现 `io.ReadWriteCloser`，按脚本返回字节流；可注入延迟、错误、分片到达。用于测试分帧、提示符、超时、重连。
- **假 Transport/Controller**：业务层与 gRPC 层测试不依赖真串口；按调用序列返回预设应答或状态。
- **假时钟/小间隔**：保活测试通过注入短间隔（如 10ms）或直接调用单步方法，避免真实等待。

### 13.3 集成与硬件验收

无硬件时，全部单元测试与 RPC 测试必须通过。硬件验收清单：

1. 拔出模块 → `NO_HW`；重新插入 → 30 秒内恢复 `OK`，串口重连成功。
2. 拔出 SIM → `NO_CARD`；插回 → 恢复。
3. 断开天线或屏蔽信号 → `NO_SIGNAL`；恢复信号 → 恢复。
4. `SetEnabled(false)` → `DISABLED`；`SetEnabled(true)` → 复位并重新拨号，最终 `OK`。
5. 短信：11 位、`+86`、`086` 三种输入；单条 70 字、71 字（2 片）、1072 字（16 片）均能收到且内容正确；1073 字返回 `InvalidArgument`。
6. 语音：EM05-CE/CN 可拨通并播放 TTS，播放结束后挂断；无人接听 15 秒超时并挂断；Fibocom 返回 `FailedPrecondition`。
7. 保活：制造无卡/无信号，确认状态变化日志与检测间隔符合配置；确认源码中无 reboot 调用。失败阈值分支因结构性不可达，不做运行时验证（见 10.4）。
8. 复位命令：验证 `AT^reset` 在目标固件上的行为；若返回 ERROR，记录并评审备选命令。
9. gRPC：grpcurl 调用全部方法；非回环监听时启动日志有 WARN。
10. systemd：开机自启、异常退出自动拉起、SIGTERM 优雅退出（5 秒内）。

### 13.4 质量门禁

```bash
go build ./...
go vet ./...
go test -race ./...
```

三项全部通过方可进入硬件验收。PDU 黄金用例失败视为阻断。

---

## 14. 实施计划

| 阶段 | 交付物 | 退出标准 |
| --- | --- | --- |
| P0 骨架 | `go.mod`、目录、日志、config 加载与校验、Makefile | `go build`、config 单测通过 |
| P1 PDU | `internal/pdu` 完整实现与单测 | 黄金向量、边界、上限用例通过 |
| P2 AT 层 | 串口会话、分帧、执行器、重连、假串口测试 | 假串口覆盖 OK/ERROR/超时/提示/重连 |
| P3 状态与 gRPC | modem 状态检测、proto、gRPC server、`GetStatus`/`SetLogLevel` | bufconn 测试通过；真机 `GetStatus` 正确 |
| P4 短信 | 号码归一化、PDU 拼装、发送流程、`SendSMS` | 单/多片真机收发成功；失败路径可观测 |
| P5 语音 | 呼叫、CLCC 解析、QWTTS、挂断、`SendVoice` | 真机播放成功；超时/取消能挂断 |
| P6 使能与保活 | 使能/复用/重拨、keepalive 阈值、日志 | 阈值单测通过；真机使能/禁用验证 |
| P7 部署验收 | systemd、udev、部署文档、硬件清单 | 13.3 清单全部通过 |

每个阶段完成后运行质量门禁；阶段之间不引入跨阶段未完成依赖。

---

## 15. 验收标准

1. 功能：状态检测、使能/禁用、短信（单/长）、语音、保活行为符合第 6–10 章。
2. 接口：第 9 章 proto 全部方法可用；错误映射与 9.3 一致；生成代码可编译。
3. 并发：并发短信/语音请求串行执行；取消能及时返回；语音取消后执行挂断。
4. 可靠性：串口断开后自动重连；断线期间返回 `NO_HW` 而非阻塞。
5. 安全：默认只监听回环；无 shell 子进程；不自动重启系统；日志不含完整手机号与正文（非 DEBUG）。
6. 可测试：`go test -race ./...` 全绿；PDU 黄金用例通过。
7. 部署：systemd 托管，开机自启，权限最小化；配置校验失败快速退出。
8. 文档：本文档与实现一致；行为差异与开放问题有记录。

---

## 16. 与 C 版行为差异

| 项 | C 版 | Go 版 | 原因 |
| --- | --- | --- | --- |
| 串口生命周期 | 每条命令 open/close | 常驻 + 自动重连 | 性能、稳定性、URC/缓冲区问题 |
| 配置 | UCI + `uci get` 子进程 | YAML，严格校验 | 脱离 OpenWrt |
| 外部命令 | `cat`/`ping`/`reboot`/`uci` | 无 shell 子进程 | 可移植、安全 |
| 接口 | ubus 四方法 | gRPC 五方法（新增 `GetStatus`、`SetLogLevel`） | 新调用约定 |
| 结果返回 | 状态字符串 | 枚举 + detail | 类型安全 |
| 发送失败结果 | 返回 -1，`errCode` 可能仍为 OK | `OPERATION_FAILED` + detail | 调用方需明确失败原因 |
| 保活间隔 | 固定 20s，配置失效 | 按配置生效 | 修复缺陷 |
| 失败阈值 | 重拨 6 → 复位 → reboot | 重拨 6 → 重新初始化；超阈值停止并告警 | 移除 reboot |
| 保活失败分支 | 不可达 | 保留且仍不可达（结构性） | 评审决定照抄逻辑 |
| 模块 Reset | `AT^reset` 后立即继续 | `AT^reset` 后等待模块恢复（≤60s） | 常驻连接需要等待 |
| 状态解析 | `sscanf`/固定下标/子串 | 正则/字段解析 | 可靠性 |
| 发送前状态检查 | 使用保活缓存的状态 | 实时检测 | 避免使用过期状态 |
| 短信 DA 长度 | 固定 `0D` | 按号码计算 | 支持其他长度 |
| 短信超长 | 可能写越界 | 1072 字符上限，超限报错 | 安全 |
| 短信迟到 OK | 1 秒后重读缓冲 | 依赖 30 秒应答超时 | 简化，串口常驻 |
| 语音硬件检查 | 函数存在但未调用 | 前置检查 | 明确错误 |
| 通话接通判定 | 子串 `"0,0,0"` | 解析 `+CLCC` 的 `stat` 字段 | 可靠性 |
| 语音文本超长 | 1024 字节命令缓冲截断 | 480 字符上限并报错 | 避免静默截断 |
| debug 开关 | ubus `debug` | `-debug` / `SetLogLevel` | 新接口 |
| coredump | 进程内 `setrlimit` | 移除，交由系统策略 | 职责分离 |
| ping 连通性 | 代码已注释 | 不实现 | 非目标 |
| 模块类型判定 | 硬编码路径 | `module.version_file` 可配置 | 通用 Linux |
| 短信号码格式 | 支持 11 位、86 开头 13 位、`+86`、`086` | 增加 `0086` 支持 | 兼容更多输入 |
| 配置项 | 主/备 ping 服务器 | 删除 | 对应功能不存在 |
| 单实例 | `O_EXCL` | 部署层保证 | 串口库限制 |

---

## 17. 风险与开放问题

| 风险/问题 | 影响 | 处理 |
| --- | --- | --- |
| `AT^reset` 为华为语法，Quectel/Fibocom 固件可能不支持 | 使能流程复位失败 | 硬件验收确认；备选 `AT+CFUN=1,1`，变更需更新文档与测试 |
| `AT+ipr` 修改模块波特率后可能失联 | 复位后串口通信异常 | 与 C 行为一致；硬件验收确认；必要时重新打开串口或移除该命令 |
| 模块固件应答差异（`>` 提示、`+CLCC` 字段、`+QWTTS`） | 流程误判 | 硬件矩阵验收；解析器容错并记录原始应答 |
| 保活失败分支不可达 | 自动重拨/重新初始化实际不触发 | 已按评审照抄；无法通过状态伪造触发（枚举顺序导致提前返回）；启用前需调整失败判定 |
| RSSI=16 实测可发送短信 | 原 C 规则会误报 `NO_SIGNAL` | 已调整为仅 RSSI < 16 判无信号 |
| 短信发送非事务性 | 多片部分发送后失败 | 文档与返回 `detail` 明确；调用方决定是否重发 |
| 短信/语音进行中无 URC 处理 | 依赖轮询，无法实时感知 | 当前业务均为同步轮询；后续需求再设计 URC 分发 |
| 模块重新枚举后 tty 编号变化 | 重连失败（`NO_HW`） | 运维确认设备路径；可用 udev 固定符号链接 |
| 无 TLS、无鉴权 | 若监听非回环将被未授权调用 | 默认回环；非回环启动 WARN；跨机使用 SSH 隧道或另立 TLS 需求 |
| 串口库 `Close` 能否中断阻塞读 | 关停可能延迟 | 硬件与库版本验证；必要时设置读超时轮询 |
| 语音 TTS 最大 480 字符 | 长文本被拒绝 | 明确返回 `InvalidArgument`；业务侧拆分 |
| 模块路径与仓库路径未最终确定 | import/go_package 需调整 | 迁移仓库时统一替换并重新生成 |
| 短信中心依赖模块默认 CSCA | 特殊卡可能发送失败 | 失败日志含 CMS 错误；必要时新增 SMSC 配置（本期不做） |
| 不支持短信回执/接收 | 业务能力受限 | 非目标；另立需求 |

开放问题（需后续决策，不阻塞本期）：

1. 是否调整保活失败判定，使自愈逻辑真正可达（当前 `DIAL_FAIL`/`BUSY` 被 `status < OK` 分支提前捕获；可增加显式可恢复失败判断）。
2. 是否增加 PDP 拨号状态检查（`AT+CGACT?`），为 `DIAL_FAIL` 提供实际来源。
3. 是否增加手动 `Reset` RPC 供运维使用（当前只能通过 `SetEnabled` 触发复位）。
4. 是否将语音文本上限从 480 字符调整为模块实际能力。
5. 是否支持配置热加载（SIGHUP）。
6. 目标仓库的最终模块路径。

---

## 附录 A：AT 命令清单

| 命令 | 用途 | 使用位置 | 默认超时 |
| --- | --- | --- | --- |
| `AT` | 模块探活 | 状态检测 | 3s |
| `ATE0` | 关闭回显 | 初始化/复位 | 3s |
| `AT+CPIN?` | SIM 卡状态 | 状态检测 | 3s |
| `AT+CSQ` | 信号质量 | 状态检测 | 3s |
| `AT+CPAS` | 模块活动状态 | 发送前忙检测 | 3s |
| `AT+COPS?` | 运营商 | 短信前判断电信卡 | 3s |
| `AT+cgact=0,1` | 去激活 PDP | 重拨 | 3s |
| `AT+cgact=1,1` | 激活 PDP | 重拨 | 3s |
| `AT^reset` | 模块复位 | 使能/禁用 | 3s（之后等待恢复 60s） |
| `AT+cgdcont=1,"IP","<apn>"` | 设置 APN | 复位 | 3s |
| `AT+qnetdevctl=1,1,1` | EM05 数据模式配置 | 复位（仅 EM05） | 3s |
| `AT+ipr=<baud>` | 设置模块波特率 | 复位 | 3s |
| `AT+CMGF=0` | 短信 PDU 模式 | 短信 | 3s |
| `AT+CMEE=2` | 详细错误码 | 短信 | 3s |
| `AT+QCFG="ltectcc/smsstorage",1` | 电信卡短信存储 | 短信（仅 CHN-CT） | 3s |
| `AT+CMGS=<len>` | 发送短信 | 短信 | 5s 等待 `>` |
| `<PDU>\x1A` | 提交短信 PDU | 短信 | 30s |
| `ATD<number>;` | 语音拨号 | 语音 | 30s |
| `AT+CLCC` | 通话列表 | 语音 | 3s |
| `AT+QWTTS=1,1,1,"<hex>"` | TTS 播放 | 语音 | 10s |
| `AT+QWTTS?` | TTS 状态 | 语音 | 3s |
| `ATH` | 挂断 | 语音 | 5s |

命令大小写与 C 保持一致；AT 命令本身大小写不敏感。

---

## 附录 B：状态映射

| C 枚举 | C 字符串 | Go 枚举 | gRPC 枚举 |
| --- | --- | --- | --- |
| `GSM_STATUS_DISABLED` | `HW Disabled` | `StatusDisabled` | `GSM_STATUS_DISABLED` |
| `GSM_STATUS_NO_HW` | `No hardware` | `StatusNoHW` | `GSM_STATUS_NO_HW` |
| `GSM_STATUS_NO_CARD` | `No sim-card` | `StatusNoCard` | `GSM_STATUS_NO_CARD` |
| `GSM_STATUS_NO_SIG` | `No signal` | `StatusNoSignal` | `GSM_STATUS_NO_SIGNAL` |
| `GSM_STATUS_DIAL_FAIL` | `Dial fail` | `StatusDialFail` | `GSM_STATUS_DIAL_FAIL` |
| `GSM_STATUS_BUSY` | `Busy` | `StatusBusy` | `GSM_STATUS_BUSY` |
| `GSM_STATUS_OK` | `OK` | `StatusOK` | `GSM_STATUS_OK` |
| — | — | `StatusOperationFailed` | `GSM_STATUS_OPERATION_FAILED` |

状态检测可返回值：`DISABLED`、`NO_HW`、`NO_CARD`、`NO_SIGNAL`、`OK`。`DIAL_FAIL`、`BUSY`、`OPERATION_FAILED` 由业务操作产生或保留。

---

## 附录 C：参考资料

- 3GPP TS 23.040：Technical realization of the Short Message Service (SMS)
- 3GPP TS 27.005：Use of DTE-DCE interface for SMS and CBS
- 3GPP TS 27.007：AT command set for User Equipment (UE)
- Quectel EM05 系列 AT 命令手册（硬件验收时以实际固件文档为准）
- Go 标准库：`log/slog`、`context`
- `google.golang.org/grpc` 文档
- `go.bug.st/serial` 文档

---

*本文档为设计基线。实现过程中如发现与硬件行为不符，须先更新本文档并评审，再修改代码。*

---

## 附录 D：模块能力差异

| 能力 | EM05-CE/CN | Fibocom L610 |
| --- | --- | --- |
| 数据重拨 | `AT+cgact=0,1` / `AT+cgact=1,1` | `AT+GTRNDIS=0,1` / `AT+GTRNDIS=1,1` |
| 数据模式配置 | `AT+qnetdevctl=1,1,1` | 不发送 Quectel 专用命令 |
| 复位 | `AT^reset`，失败回退 `AT+CFUN=1,1` | `AT+CFUN=15`，失败回退 `AT+CFUN=1,1` |
| 电信短信存储 | `AT+QCFG="ltectcc/smsstorage",1` | 不发送 Quectel 专用命令 |
| QWTTS 文本语音 | 支持 | 当前接口不支持 |
| 运营商语音限制 | EM05-CE 仅移动；CN A03 不支持联通 VoLTE | 不适用 |

详细配置和验收说明见 `module-compatibility.md`。
