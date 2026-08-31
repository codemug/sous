PROTOC_GEN_GO_VERSION := v1.34.2
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1

.PHONY: proto
proto:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	protoc \
		--go_out=. --go_opt=module=github.com/codemug/sous \
		--go-grpc_out=. --go-grpc_opt=module=github.com/codemug/sous \
		proto/souslet/v1/souslet.proto

.PHONY: test
test:
	go test ./...
