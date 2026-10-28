VERSION ?= dev
COMMIT ?= unknown
DATE ?= unknown
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build test vet proto clean install

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/gsmd ./cmd/gsmd

test:
	go test -race ./...

vet:
	go vet ./...

proto:
	protoc -I api/proto --go_out=api/gen --go_opt=paths=source_relative --go-grpc_out=api/gen --go-grpc_opt=paths=source_relative api/proto/pess/gsm/v1/gsm.proto

clean:
	go clean
	rm -rf bin

install: build
	install -Dm0755 bin/gsmd /usr/sbin/gsmd
	install -Dm0644 configs/gsm.yaml /etc/gsm/gsm.yaml
	install -Dm0644 deploy/gsmd.service /etc/systemd/system/gsmd.service
