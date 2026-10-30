# gsmd

PESS 4G 模块管理服务（Go 实现），对应设计基线 [docs/gsm-go-dev.md](docs/gsm-go-dev.md)。

## 能力

- 4G 模块状态检测：禁用、无硬件、无 SIM、无信号、正常
- 支持 EM05-CE、EM05-CN A03/A04、Fibocom L610
- 使能/禁用、PDP/RNDIS 重拨、模块复位、串口重新初始化
- UCS2 PDU 短信，支持长短信分片
- EM05-CE/CN 语音外呼与 QWTTS 文本播放
- 串口常驻、断线退避重连、业务操作串行化
- gRPC + reflection，YAML 严格配置，systemd 托管

模块差异和配置值见 [docs/module-compatibility.md](docs/module-compatibility.md)。

第三方 gRPC 接入见 [docs/integration.md](docs/integration.md)。

## 与现场其它组件的边界

部署前必须确认这三件事，否则会和现场已有服务互相踩：

1. **AT 口只能有一个持有者。** `serial.device: auto` 时按 udev 的稳定名 `/dev/serial/by-id/*-if02-port0` 找 AT 口（EM05 上是 `ttyUSB2`，L610 上是 `ttyUSB0`）。串口库打开该设备时会设置 TIOCEXCL，非 root 进程再打开会拿到 EBUSY。
   现场若还有其它脚本开机抢这个口（典型是厂商的 `4G-info-get.service` / `get_4g_version.sh`，
   它会 `cat /dev/ttyUSB2` 并发 `AT+QTTSEtup`），必须先停用它：
   `systemctl disable --now 4G-info-get.service`。版本号由本服务实测 `/ATI`、`AT+CGMM`、`AT+CGMR`
   后写回 `/etc/mmdc/4g_version`（`module.version_file_write`），所以换模块也**不需要手工删文件**：
   `module.type=auto` 时以 AT 实测结果为准，文件与实测不一致会覆盖并记一条 warn。
   root 带 CAP_SYS_ADMIN 能绕过 TIOCEXCL，所以"停掉那个脚本"是必需步骤，不是可选项。
2. **数据拨号不归本服务。** QMI 模式（`AT+QCFG="usbnet"` 为 0）下拨号由 `quectel-CM`
   （现场 `quectel-cm.service`）负责；本服务只做模块状态、短信、语音和模块控制。
   `module.data_mode=auto` 会自动识别，也可显式写 `external`/`at`。
3. **中文语音库。** 语音播报依赖模块的中文 TTS 库，本服务启动后会用 `AT+QTTSETUP=2,3`
   自检，发现是英文库会切到中文库并提示需要复位一次才生效。

## 构建与测试

```bash
make test
make vet
make build
```

竞态测试：

```bash
CGO_ENABLED=1 go test -race ./...
```

交叉编译（现场是 aarch64，直接生成静态包，无需板上装 Go）：

```bash
make build-linux ARCH=arm64     # 产物 bin/gsmd-linux-arm64
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o bin/gsmd-amd64 ./cmd/gsmd
```

生成 gRPC 代码需要 `protoc`、`protoc-gen-go`、`protoc-gen-go-grpc`。当前产物由 protoc 29.3、protoc-gen-go 1.34.2、protoc-gen-go-grpc 1.5.1 生成：

```bash
make proto
```

## 运行

```bash
./bin/gsmd -config configs/gsm.yaml -debug
```

默认 gRPC 监听 `127.0.0.1:50051`，并注册 server reflection：

```bash
grpcurl -plaintext 127.0.0.1:50051 gsm.v1.GsmService/GetStatus
grpcurl -plaintext -d '{"phone_number":"13612345678","message":"测试短信"}' \
  127.0.0.1:50051 gsm.v1.GsmService/SendSMS
```

## 部署

```bash
sudo useradd -r -s /usr/sbin/nologin -G dialout gsmd || true
sudo make install
sudo systemctl daemon-reload
sudo systemctl enable --now gsmd
journalctl -u gsmd -f
```

生产配置位于 `/etc/gsm/gsm.yaml`。串口设备、APN 和模块类型需按现场调整；`apn: auto` 会按 SIM 的 IMSI 自动选移动 `cmnet` / 联通 `3gnet` / 电信 `ctnet`，填具体值时以配置为准。`make install` 只在
`/etc/gsm/gsm.yaml` 不存在时写入，升级不会覆盖现场配置。非回环监听会记录安全告警，且该服务本身不提供 TLS 或鉴权。

首次部署顺序：

```bash
sudo useradd -r -s /usr/sbin/nologin -G dialout gsmd || true
sudo systemctl disable --now 4G-info-get.service     # 停掉抢 AT 口的厂商脚本
sudo chown gsmd:gsmd /etc/mmdc/4g_version            # 让 gsmd 能维护版本缓存
sudo make install ARCH=arm64
sudo systemctl daemon-reload
sudo systemctl enable --now gsmd
journalctl -u gsmd -f
```


## 验收说明

- `internal/pdu` 已覆盖文档中的黄金 PDU、70/71/1072/1073 字符边界。
- 无硬件环境已覆盖状态、短信、语音、取消挂断、gRPC、模块识别和保活单测。
- L610 的拨号、复位和短信命令已按广和通 MTC 拨号指南实现。
- 真机仍需验证各模块固件版本、运营商组合、短信中心、SIM/天线和 QWTTS 中文库。
- 短信 PDU 的 `AT+CMGS` 长度按“PDU 总字节数减去 SMSC 字段长度”计算；SMSC 为 `00` 时再减 1 字节。
