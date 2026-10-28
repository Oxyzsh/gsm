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

交叉编译：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o bin/gsmd-amd64 ./cmd/gsmd
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o bin/gsmd-arm64 ./cmd/gsmd
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
grpcurl -plaintext 127.0.0.1:50051 pess.gsm.v1.GsmService/GetStatus
grpcurl -plaintext -d '{"phone_number":"13612345678","message":"测试短信"}' \
  127.0.0.1:50051 pess.gsm.v1.GsmService/SendSMS
```

## 部署

```bash
sudo useradd -r -s /usr/sbin/nologin -G dialout gsmd || true
sudo make install
sudo systemctl daemon-reload
sudo systemctl enable --now gsmd
journalctl -u gsmd -f
```

生产配置位于 `/etc/gsm/gsm.yaml`。串口设备、APN 和模块类型需按现场调整。非回环监听会记录安全告警，且该服务本身不提供 TLS 或鉴权。

## 验收说明

- `internal/pdu` 已覆盖文档中的黄金 PDU、70/71/1072/1073 字符边界。
- 无硬件环境已覆盖状态、短信、语音、取消挂断、gRPC、模块识别和保活单测。
- L610 的拨号、复位和短信命令已按广和通 MTC 拨号指南实现。
- 真机仍需验证各模块固件版本、运营商组合、短信中心、SIM/天线和 QWTTS 中文库。
- 短信 PDU 的 `AT+CMGS` 长度按 PDU 十六进制字符数除以 2 计算，与文档黄金用例和现场样例一致。
