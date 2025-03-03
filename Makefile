SHELL=/usr/bin/env bash

GO_BUILD_IMAGE?=golang:1.15

# VERSION is the nearest tag plus number of commits and short form of most recent commit since the tag, if any
VERSION=$(shell git describe --always --tag --dirty)

unexport GOFLAGS

CLEAN:=
BINS:=

GOFLAGS:=

.PHONY: all
all: build

## FFI
FFI_PATH:=extern/filecoin-ffi/
FFI_DEPS:=.install-filcrypto
FFI_DEPS:=$(addprefix $(FFI_PATH),$(FFI_DEPS))

$(FFI_DEPS): build/.filecoin-install ;

build/.filecoin-install: $(FFI_PATH)
	$(MAKE) -C $(FFI_PATH) $(FFI_DEPS:$(FFI_PATH)%=%)
	@touch $@

MODULES+=$(FFI_PATH)
BUILD_DEPS+=build/.filecoin-install
CLEAN+=build/.filecoin-install

ffi-version-check:
	@[[ "$$(awk '/const Version/{print $$5}' extern/filecoin-ffi/version.go)" -eq 3 ]] || (echo "FFI version mismatch, update submodules"; exit 1)
BUILD_DEPS+=ffi-version-check

.PHONY: ffi-version-check

$(MODULES): build/.update-modules ;
# dummy file that marks the last time modules were updated
build/.update-modules:
	git submodule update --init --recursive
ifneq ($(FFI_COMMIT_HASH),"")
	git submodule update --init --recursive && cd extern/filecoin-ffi/ && git checkout -q $(FFI_COMMIT_HASH)
endif
	touch $@

CLEAN+=build/.update-modules

# Add version information to the package
ldflags=-X=main.appVersion=$(VERSION)
ifneq ($(strip $(LDFLAGS)),)
	ldflags+=-extldflags=$(LDFLAGS)
endif
GOFLAGS+=-ldflags="$(ldflags)"

# suppress a warning generated by lmdb
# export CGO_CFLAGS+=-Wno-stringop-overflow

.PHONY: build
build: deps estuary shuttle barge benchest bsget shuttle-proxy

.PHONY: deps
deps: $(BUILD_DEPS)

.PHONY: estuary
estuary:
	go build $(GOFLAGS)
BINS+=estuary

.PHONY: shuttle
shuttle:
	go build $(GOFLAGS) -o estuary-shuttle ./cmd/estuary-shuttle
BINS+=estuary-shuttle

.PHONY: barge
barge:
	go build $(GOFLAGS) -o barge ./cmd/barge
BINS+=barge

.PHONY: benchest
benchest:
	go build $(GOFLAGS) -o benchest ./cmd/benchest
BINS+=benchest

.PHONY: bsget
bsget:
	go build $(GOFLAGS) -o bsget ./cmd/bsget
BINS+=bsget

.PHONY: shuttle-proxy
shuttle-proxy:
	go build $(GOFLAGS) -o shuttle-proxy ./cmd/shuttle-proxy
BINS+=shuttle-proxy

.PHONY: install
install: estuary
	@install -C estuary /usr/local/bin/estuary

.PHONY: install-shuttle
install-shuttle: shuttle
	@install -C estuary-shuttle /usr/local/bin/estuary-shuttle

.PHONY: install-estuary-service
install-estuary-service:
	cp scripts/estuary-service/estuary-setup.service /etc/systemd/system/estuary-setup.service
	cp scripts/estuary-service/estuary.service /etc/systemd/system/estuary.service
	mkdir -p /etc/estuary
	cp scripts/estuary-service/config.env /etc/estuary/config.env
	mkdir -p /var/log/estuary
	cp scripts/estuary-service/log.env /etc/estuary/log.env

	#TODO: if service changes to estuary user/group, need to chown the /etc/estuary dir and contents

	systemctl daemon-reload

	#Edit config values in /etc/estuary/config.env before running any estuary service files
	#Run 'sudo systemctl start estuary-setup.service' to complete setup
	#Run 'sudo systemctl enable --now estuary.service' once ready to enable and start estuary service


.PHONY: install-estuary-shuttle-service
install-estuary-shuttle-service:
	cp scripts/est-shuttle-service/estuary-shuttle.service /etc/systemd/system/estuary-shuttle.service
	mkdir -p /etc/estuary-shuttle
	cp scripts/est-shuttle-service/config.env /etc/estuary-shuttle/config.env
	mkdir -p /var/log/estuary-shuttle
	cp scripts/est-shuttle-service/log.env /etc/estuary-shuttle/log.env

	#TODO: if service changes to estuary user/group, need to chown the /etc/estuary dir and contents

	systemctl daemon-reload

	#Edit config values in /etc/estuary/config.env before running any estuary service files
	#Run 'sudo systemctl start estuary-setup.service' to complete setup
	#Run 'sudo systemctl enable --now estuary.service' once ready to enable and start estuary service

.PHONY: clean
clean:
	rm -rf $(CLEAN) $(BINS)

.PHONY: dist-clean
dist-clean:
	git clean -xdff
	git submodule deinit --all -f

calibnet: GOFLAGS+=-tags=calibnet
calibnet: build

.PHONY: test
test:
	go test $(GOFLAGS) -v ./...

.PHONY: generate-swagger
generate-swagger:
	scripts/swagger/swag.sh

lint: ## Lint Golang files
	golint -set_exit_status ./...

test-coverage: ## Run tests with coverage
	go test -short -coverprofile cover.out -covermode=atomic ./...
	cat cover.out >> coverage.txt