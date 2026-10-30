# gsmd 第三方对接指南

## 1. 对接方式

第三方通过 gRPC 调用 `gsmd`，不要直接操作 `/dev/ttyUSB*`。

- 默认地址：`127.0.0.1:50051`
- 传输：HTTP/2 + gRPC
- 消息：Protocol Buffers 3
- 权威接口文件：`api/proto/gsm/v1/gsm.proto`
- 服务名：`gsm.v1.GsmService`
- 不提供 HTTP JSON、REST 或 ubus 接口

`gsmd` 管理模块状态、短信、语音和模块控制；数据拨号仍由现场模块拨号工具或系统网络配置负责。

## 2. 安全边界

服务默认只监听回环地址，且不包含 TLS、mTLS 或鉴权。

本机进程直接连接：

```text
127.0.0.1:50051
```

跨机器接入优先使用 SSH 隧道：

```bash
ssh -N -L 50051:127.0.0.1:50051 root@设备地址
```

然后第三方仍连接本机 `127.0.0.1:50051`。不要直接把未鉴权的 gRPC 端口暴露到公网。确有跨机直连需求时，必须在网络层增加 ACL、mTLS 网关或其他受控代理。

## 3. RPC 清单

| RPC | 用途 | 建议 deadline |
| --- | --- | --- |
| `GetStatus` | 查询模块实时状态、信号、模块和固件 | 15s |
| `SetEnabled` | 使能或禁用模块，可能触发复位和重拨 | 120s |
| `SendSMS` | 发送单条或分片短信 | 典型 120s；16 片最坏 600s |
| `SendVoice` | EM05-CE/CN 外呼并播放 QWTTS 文本 | 120s |
| `SetLogLevel` | 修改服务运行日志级别 | 5s |

`GetStatus` 也是业务健康检查入口，不另提供 HTTP 健康接口。

## 4. 状态语义

业务状态通过响应中的 `GsmStatus` 返回，RPC 本身仍为成功：

| 状态 | 含义 | 调用方处理 |
| --- | --- | --- |
| `GSM_STATUS_OK` | 模块、SIM、信号正常 | 继续业务 |
| `GSM_STATUS_DISABLED` | 模块被主动禁用 | 不应发送短信/语音 |
| `GSM_STATUS_NO_HW` | 串口或模块不存在 | 检查模块、线缆、串口配置 |
| `GSM_STATUS_NO_CARD` | SIM 未就绪 | 检查 SIM |
| `GSM_STATUS_NO_SIGNAL` | 信号弱或未知 | 检查天线、运营商 |
| `GSM_STATUS_DIAL_FAIL` | 拨号失败，当前保留 | 记录并上报 |
| `GSM_STATUS_BUSY` | 模块忙 | 稍后重试 |
| `GSM_STATUS_OPERATION_FAILED` | AT 操作失败 | 记录 `detail`，按业务策略重试 |

`GetStatusResponse` 还包含：

- `rssi`：CSQ 信号值，`-1` 表示未知
- `ber`：误码率，`-1` 表示未知
- `hw_type`：`em05-ce`、`em05-cn`、`fibocom` 或 `unknown`
- `firmware`：`A03`、`A04`、`L610` 或 `unknown`
- `checked_at`：检测时间

## 5. gRPC 错误

| gRPC code | 场景 |
| --- | --- |
| `InvalidArgument` | 手机号、短信或语音文本格式错误 |
| `FailedPrecondition` | 模块不支持语音，或运营商与固件组合不支持语音 |
| `DeadlineExceeded` | 调用方 deadline 到期 |
| `Canceled` | 调用方取消 |
| `Unavailable` | 服务正在关闭 |
| `Internal` | 未预期的内部错误 |

调用方必须同时处理“RPC 错误”和“RPC 成功但业务状态异常”两种情况。

## 6. grpcurl 快速验证

服务在回环监听时注册了 reflection：

```bash
grpcurl -plaintext 127.0.0.1:50051 list
grpcurl -plaintext 127.0.0.1:50051 gsm.v1.GsmService/GetStatus
```

发送短信：

```bash
grpcurl -plaintext \
  -d '{"phone_number":"13612345678","message":"测试短信"}' \
  127.0.0.1:50051 \
  gsm.v1.GsmService/SendSMS
```

语音：

```bash
grpcurl -plaintext \
  -d '{"phone_number":"13612345678","message":"告警测试"}' \
  127.0.0.1:50051 \
  gsm.v1.GsmService/SendVoice
```

禁用：

```bash
grpcurl -plaintext \
  -d '{"enabled":false}' \
  127.0.0.1:50051 \
  gsm.v1.GsmService/SetEnabled
```

## 7. Go 调用示例

从 `api/proto` 生成代码，或直接使用本仓库的 `api/gen` 包。

```go
package main

import (
    "context"
    "fmt"
    "time"

    gsmv1 "gsm/api/gen/gsm/v1"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
)

func main() {
    conn, err := grpc.NewClient(
        "127.0.0.1:50051",
        grpc.WithTransportCredentials(insecure.NewCredentials()),
    )
    if err != nil {
        panic(err)
    }
    defer conn.Close()

    client := gsmv1.NewGsmServiceClient(conn)
    ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()

    status, err := client.GetStatus(ctx, &gsmv1.GetStatusRequest{})
    if err != nil {
        panic(err)
    }
    fmt.Printf("status=%s hw=%s firmware=%s detail=%s\n",
        status.GetStatus(), status.GetHwType(), status.GetFirmware(), status.GetDetail())

    if status.GetStatus() != gsmv1.GsmStatus_GSM_STATUS_OK {
        return
    }

    smsCtx, smsCancel := context.WithTimeout(context.Background(), 2*time.Minute)
    defer smsCancel()
    sms, err := client.SendSMS(smsCtx, &gsmv1.SendSMSRequest{
        PhoneNumber: "13612345678",
        Message:     "测试短信",
    })
    if err != nil {
        panic(err)
    }
    if sms.GetStatus() != gsmv1.GsmStatus_GSM_STATUS_OK {
        panic(fmt.Sprintf("SMS failed: %s %s", sms.GetStatus(), sms.GetDetail()))
    }
}
```

如果第三方仓库不能直接导入 `gsm`，可复制 `.proto` 并用自定义 `go_package` 重新生成，消息字段和服务语义保持不变。

## 8. Python 调用示例

安装依赖：

```bash
pip install grpcio grpcio-tools protobuf
```

生成代码：

```bash
python -m grpc_tools.protoc \
  -I api/proto \
  --python_out=gen \
  --grpc_python_out=gen \
  api/proto/gsm/v1/gsm.proto
```

调用：

```python
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent / "gen"))

import grpc
from gsm.v1 import gsm_pb2, gsm_pb2_grpc

with grpc.insecure_channel("127.0.0.1:50051") as channel:
    stub = gsm_pb2_grpc.GsmServiceStub(channel)
    status = stub.GetStatus(gsm_pb2.GetStatusRequest(), timeout=15)
    print(status.status, status.hw_type, status.firmware, status.detail)

    if status.status == gsm_pb2.GSM_STATUS_OK:
        response = stub.SendSMS(
            gsm_pb2.SendSMSRequest(
                phone_number="13612345678",
                message="测试短信",
            ),
            timeout=120,
        )
        print(response.status, response.segments, response.detail)
```

## 9. 调用约束与重试建议

- 发送短信前先调用 `GetStatus`，只处理 `GSM_STATUS_OK` 和明确的业务异常。
- `BUSY` 建议退避后重试，不要立即循环。
- `NO_HW`、`NO_CARD`、`NO_SIGNAL` 不要通过重复发送短信来恢复，应上报设备故障。
- `DeadlineExceeded` 不保证请求没有执行；短信可能已部分发送。
- 当前接口没有幂等键，短信发送不是事务性的。调用方重试前应使用业务流水号自行去重。
- 并发请求会在服务内串行排队，排队时间计入调用方 deadline。
- 取消语音时服务会尽量执行 `ATH` 挂断。
- 语音号码必须是 11 位 ASCII 数字；短信支持 11 位、`+86`、`086`、`0086` 和 13 位国家码格式。
- L610 当前支持状态、短信、数据拨号控制和复位，但 QWTTS 文本语音返回 `FailedPrecondition`。

## 10. 上线检查

1. 确认 `gsmd` 正常运行，`GetStatus` 返回 `GSM_STATUS_OK`。
2. 确认 `hw_type` 和 `firmware` 与现场模块一致。
3. 用 `grpcurl` 或示例客户端完成状态、短信测试。
4. 配置客户端 deadline、TLS/SSH 隧道和访问控制。
5. 在业务侧实现 SMS 去重、故障分类和告警。

## 11. go-pess 告警短信/电话接入

go-pess 的告警通知层已支持直接调用本机 Go `gsmd`：

- `personal_sms` → `gsm.v1.GsmService/SendSMS`
- `personal_voice` → `gsm.v1.GsmService/SendVoice`

默认 gRPC 地址：

```text
127.0.0.1:50051
```

告警设置中的“GSM gRPC 服务地址”为空时使用默认地址；也可以填写 `host:port`。如果填写 `http://` 或 `https://` 地址，则保留旧 C 版 `/ubus/call/gsm` HTTP JSON-RPC 兼容模式。环境变量 `GSM_GRPC_ADDR` 可在设置为空时覆盖默认地址。

注意：

- 短信通知最大保留 1072 个 BMP 字符。
- 电话通知号码会归一化为 11 位数字，文本最多保留 480 个 BMP 字符。
- gRPC 调用成功但业务状态不是 `GSM_STATUS_OK` 时，仍按发送失败记录。
- 短信无幂等键，告警重试可能造成重复短信。
