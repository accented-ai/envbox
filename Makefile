PROJECT_ROOT := $(shell git rev-parse --show-toplevel)
GO_SOURCE_FILES := $(shell git ls-files '*.go')
GO_FILES := $(shell git ls-files '*.go' '*.sum')
EMBED_FILES := cli/wrap_dockerd.sh
IMAGE_FILES := $(shell find deploy)
IMAGE_OUTPUT ?= --load
IMAGE_TAG ?= envbox
ARCH ?= linux/$(shell go env GOARCH)
GOOS := $(word 1,$(subst /, ,$(ARCH)))
GOARCH := $(word 2,$(subst /, ,$(ARCH)))
SYSBOX_SOURCE_LOCK := deploy/sysbox-source.lock
include $(SYSBOX_SOURCE_LOCK)
SYSBOX_SHA ?= $(SYSBOX_SHA_$(GOOS)_$(GOARCH))
SYSBOX_FS_COMMIT := 3cdbf54598b459f7f84270c80c70fe3190704eff
SYSBOX_FS_DIR ?= ../sysbox-fs
SYSBOX_FS_REPO ?= https://github.com/accented-ai/sysbox-fs.git
SYSBOX_IPC_REPO ?= https://github.com/nestybox/sysbox-ipc.git
SYSBOX_LIBS_REPO ?= https://github.com/nestybox/sysbox-libs.git
SYSBOX_RUNC_REPO ?= https://github.com/nestybox/sysbox-runc.git

ifeq ($(SYSBOX_SHA),)
$(error unsupported architecture: $(ARCH))
endif

.PHONY: clean
clean:
	rm -rf build

.PHONY: build/envbox
build/envbox: $(GO_FILES) $(EMBED_FILES)
	mkdir -p $(@D)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o build/envbox ./cmd/envbox

.PHONY: build/image/envbox
build/image/envbox: build/image/envbox/$(GOOS)_$(GOARCH)/.ctx

build/image/envbox/$(GOOS)_$(GOARCH)/.ctx: Makefile build/envbox $(IMAGE_FILES) scripts/sysbox_fs_context.sh scripts/sysbox_source_context.sh scripts/sysbox_source_lock.sh
	./scripts/sysbox_source_lock.sh check
	rm -rf $(@D)
	mkdir -p $(@D)
	cp -r build/envbox deploy/. $(@D)
	sysbox_fs_context="$$(SYSBOX_FS_COMMIT=$(SYSBOX_FS_COMMIT) SYSBOX_FS_DIR=$(SYSBOX_FS_DIR) SYSBOX_FS_REPO=$(SYSBOX_FS_REPO) ./scripts/sysbox_fs_context.sh)"; \
		sysbox_ipc_context="$$(./scripts/sysbox_source_context.sh sysbox-ipc $(SYSBOX_IPC_REPO) $(SYSBOX_IPC_COMMIT))"; \
		sysbox_libs_context="$$(./scripts/sysbox_source_context.sh sysbox-libs $(SYSBOX_LIBS_REPO) $(SYSBOX_LIBS_COMMIT))"; \
		sysbox_runc_context="$$(./scripts/sysbox_source_context.sh sysbox-runc $(SYSBOX_RUNC_REPO) $(SYSBOX_RUNC_COMMIT))"; \
		docker buildx build \
			--build-arg SYSBOX_FS_COMMIT=$(SYSBOX_FS_COMMIT) \
			--build-arg SYSBOX_SHA=$(SYSBOX_SHA) \
			--build-arg SYSBOX_VERSION=$(SYSBOX_VERSION) \
			--build-context "sysbox-fs=$$sysbox_fs_context" \
			--build-context "sysbox-ipc=$$sysbox_ipc_context" \
			--build-context "sysbox-libs=$$sysbox_libs_context" \
			--build-context "sysbox-runc=$$sysbox_runc_context" \
			$(IMAGE_OUTPUT) \
			--tag $(IMAGE_TAG) \
			--platform $(ARCH) \
			$(@D)
	touch $@

.PHONY: check/sysbox-source-lock
check/sysbox-source-lock:
	./scripts/sysbox_source_lock.sh check

.PHONY: update/sysbox-source-lock
update/sysbox-source-lock:
	./scripts/sysbox_source_lock.sh update $(SYSBOX_VERSION)

.PHONY: fmt
fmt: fmt/go fmt/md

.PHONY: fmt/go
fmt/go:
	# VS Code users should check out
	# https://github.com/mvdan/gofumpt#visual-studio-code
	go run mvdan.cc/gofumpt@v0.4.0 -w -l $(GO_SOURCE_FILES)

.PHONY: fmt/md
fmt/md:
	go run github.com/Kunde21/markdownfmt/v3/cmd/markdownfmt@v3.1.0 -w ./README.md

.PHONY: test
test:
	go test -v -count=1 ./...

.PHONY: test-integration
test-integration:
	CODER_TEST_INTEGRATION=1 go test -v -count=1 ./integration/
