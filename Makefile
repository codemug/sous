PROTOC_VERSION := 27.0
PROTOC_GEN_GO_VERSION := v1.34.2
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1

# Determine platform and download protoc
PROTOC_BIN := $(CURDIR)/.bin/protoc
PROTOC_DOWNLOADED := $(CURDIR)/.bin/protoc-$(PROTOC_VERSION).downloaded

$(PROTOC_DOWNLOADED):
	@mkdir -p $(CURDIR)/.bin
	@OS=$$(uname -s | tr A-Z a-z); \
	ARCH=$$(uname -m); \
	case "$$ARCH" in x86_64) ARCH=x86_64;; aarch64|arm64) ARCH=aarch_64;; esac; \
	PLATFORM="$$OS-$$ARCH"; \
	URL="https://github.com/protocolbuffers/protobuf/releases/download/v$(PROTOC_VERSION)/protoc-$(PROTOC_VERSION)-$$PLATFORM.zip"; \
	echo "Downloading protoc $(PROTOC_VERSION) for $$PLATFORM..."; \
	cd $(CURDIR)/.bin && curl -sL -o protoc-$(PROTOC_VERSION).zip "$$URL" || (echo "Failed to download protoc"; exit 1); \
	unzip -q protoc-$(PROTOC_VERSION).zip && rm -f protoc-$(PROTOC_VERSION).zip; \
	chmod +x bin/protoc; \
	touch $(PROTOC_DOWNLOADED)

$(PROTOC_BIN): $(PROTOC_DOWNLOADED)
	@if [ ! -f $(PROTOC_BIN) ]; then \
		ln -s $(CURDIR)/.bin/bin/protoc $(PROTOC_BIN) || cp $(CURDIR)/.bin/bin/protoc $(PROTOC_BIN); \
	fi

.PHONY: proto
proto: $(PROTOC_BIN)
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	$(PROTOC_BIN) \
		--go_out=. --go_opt=module=github.com/codemug/sous \
		--go-grpc_out=. --go-grpc_opt=module=github.com/codemug/sous \
		proto/souslet/v1/souslet.proto

.PHONY: test
test:
	go test ./...
