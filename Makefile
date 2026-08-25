SHELL := /bin/sh

GO ?= go
NPM ?= npm
PROTOC ?= protoc
PYTHON ?= python3
VERSION ?= 1.0.1
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/xirang-grid/xirang/pkg/version.Version=$(VERSION) \
	-X github.com/xirang-grid/xirang/pkg/version.Commit=$(COMMIT) \
	-X github.com/xirang-grid/xirang/pkg/version.BuildTime=$(BUILD_TIME)

.PHONY: help fmt generate test vet build multiarch package-native contract-check openapi-check docs-check web-install web-build verify ha-drill clean

help:
	@echo "XIRANG build targets"
	@echo "  make fmt             Format Go source"
	@echo "  make generate        Regenerate checked-in Go protocol bindings"
	@echo "  make test            Run Go tests"
	@echo "  make build           Build local Go binaries"
	@echo "  make multiarch       Build linux/amd64 and linux/arm64 binaries"
	@echo "  make package-native  Create native Linux release archives"
	@echo "  make contract-check  Validate the Protobuf contract"
	@echo "  make openapi-check   Parse and validate the OpenAPI document shape"
	@echo "  make docs-check      Validate bilingual Markdown pairs and local links"
	@echo "  make web-install     Install web dependencies"
	@echo "  make web-build       Type-check and build the web console"
	@echo "  make verify          Run backend and contract checks"
	@echo "  make ha-drill        Exercise PostgreSQL active/passive recovery on loopback"

fmt:
	$(GO) fmt ./...

generate:
	$(PROTOC) -I api/proto \
		--go_out=. --go_opt=module=github.com/xirang-grid/xirang \
		--go-grpc_out=. --go-grpc_opt=module=github.com/xirang-grid/xirang \
		xirang/v1/agent.proto

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/xirangd ./cmd/xirangd
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/xirang-agent ./cmd/xirang-agent
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/xirangctl ./cmd/xirangctl
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/xirang-maint ./cmd/xirang-maint
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/xirang-mpi-adapter ./cmd/xirang-mpi-adapter
	ln -sf xirang-mpi-adapter bin/xirang-mpi-adapter-openmpi
	ln -sf xirang-mpi-adapter bin/xirang-mpi-adapter-mpich

multiarch:
	@set -eu; \
	for arch in amd64 arm64; do \
		mkdir -p "dist/linux-$$arch"; \
		for command in xirangd xirang-agent xirangctl xirang-maint; do \
			CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath \
				-ldflags "$(LDFLAGS)" -o "dist/linux-$$arch/$$command" "./cmd/$$command"; \
		done; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath \
			-ldflags "$(LDFLAGS)" -o "dist/linux-$$arch/xirang-mpi-adapter" ./cmd/xirang-mpi-adapter; \
		ln -sf xirang-mpi-adapter "dist/linux-$$arch/xirang-mpi-adapter-openmpi"; \
		ln -sf xirang-mpi-adapter "dist/linux-$$arch/xirang-mpi-adapter-mpich"; \
	done

package-native: multiarch web-build
	@set -eu; \
	rm -f dist/SHA256SUMS; \
	for arch in amd64 arm64; do \
		archive="xirang-$(VERSION)-linux-$$arch"; \
		stage="dist/$$archive"; \
		mkdir -p "$$stage/bin" "$$stage/share/xirang/web" "$$stage/deploy"; \
		cp -a dist/linux-$$arch/xirangd dist/linux-$$arch/xirang-agent dist/linux-$$arch/xirangctl dist/linux-$$arch/xirang-maint dist/linux-$$arch/xirang-mpi-adapter "$$stage/bin/"; \
		cp -R web/dist/. "$$stage/share/xirang/web/"; \
		cp -R deploy/systemd deploy/openrc deploy/install deploy/config "$$stage/deploy/"; \
		cp -R deploy/job-artifacts "$$stage/deploy/"; \
		mkdir -p "$$stage/docs"; \
		cp README.md README.zh-CN.md LICENSE THIRD_PARTY_NOTICES.md THIRD_PARTY_NOTICES.zh-CN.md "$$stage/"; \
		$(PYTHON) scripts/collect_third_party_licenses.py "$$stage/share/licenses"; \
		cp -R docs/user "$$stage/docs/"; \
		cp -R examples "$$stage/"; \
		tar -C dist -czf "dist/$$archive.tar.gz" "$$archive"; \
		(cd dist && sha256sum "$$archive.tar.gz") >> dist/SHA256SUMS; \
		rm -rf "$$stage"; \
	done; \
	chmod 0644 dist/SHA256SUMS dist/xirang-$(VERSION)-linux-*.tar.gz; \
	(cd dist && sha256sum -c SHA256SUMS); \
	rm -rf .data/package-smoke; \
	./install.sh --archive "$$(pwd)/dist/xirang-$(VERSION)-linux-amd64.tar.gz" \
		--destdir "$$(pwd)/.data/package-smoke" --skip-dependencies; \
	test "$$($$(pwd)/.data/package-smoke/usr/local/bin/xirangd -version)" = "XIRANG $(VERSION)"; \
	test -L .data/package-smoke/usr/local/bin/xirang-mpi-adapter-openmpi; \
	rm -rf .data/package-smoke

contract-check:
	mkdir -p .data/contracts
	$(PROTOC) -I api/proto --include_imports \
		--descriptor_set_out=.data/contracts/xirang-v1.pb \
		api/proto/xirang/v1/agent.proto

openapi-check:
	$(PYTHON) scripts/check_openapi.py api/openapi/xirang-v1.yaml

docs-check:
	$(PYTHON) scripts/check_docs.py

web-install:
	$(NPM) --prefix web install

web-build:
	$(NPM) --prefix web run build
	find web/dist -type d -exec chmod 0755 {} +
	find web/dist -type f -exec chmod 0644 {} +

verify: test vet contract-check openapi-check docs-check

ha-drill: build
	./scripts/ha_postgresql_drill.sh

clean:
	rm -rf bin dist .data/contracts web/dist
