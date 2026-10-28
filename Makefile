VERSION ?= dev
COMMIT ?= unknown
DATE ?= unknown
# 交叉编译目标：现场主机是 aarch64，开发机可能是 amd64 或 Windows。
ARCH ?= arm64
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build build-linux test vet proto clean install

# 本机（开发机）二进制，用于跑测试和本地调试。
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/gsmd ./cmd/gsmd

# 现场部署用的 Linux 静态二进制，按 ARCH 命名，避免和本机文件混淆。
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/gsmd-linux-$(ARCH) ./cmd/gsmd

test:
	go test -race ./...

vet:
	go vet ./...

proto:
	protoc -I api/proto --go_out=api/gen --go_opt=paths=source_relative --go-grpc_out=api/gen --go-grpc_opt=paths=source_relative api/proto/pess/gsm/v1/gsm.proto

clean:
	go clean
	rm -rf bin

# 安装：只在配置文件不存在时写入，防止升级时覆盖现场改过的 APN/串口/阈值。
install: build-linux
	install -Dm0755 bin/gsmd-linux-$(ARCH) /usr/sbin/gsmd
	install -d /etc/gsm
	test -f /etc/gsm/gsm.yaml || install -m0644 configs/gsm.yaml /etc/gsm/gsm.yaml
	install -Dm0644 deploy/gsmd.service /etc/systemd/system/gsmd.service
	@echo "提示：若还没有服务账号，先执行 useradd -r -s /usr/sbin/nologin -G dialout gsmd"
	@echo "提示：让 gsmd 能维护版本缓存，执行 chown gsmd:gsmd /etc/mmdc/4g_version"
	@echo "提示：install 不覆盖已存在的 /etc/gsm/gsm.yaml"
