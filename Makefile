PLUGIN_NAME := anti-model-fallback
VERSION := 0.1.0
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

ifeq ($(GOOS),darwin)
EXT := dylib
else ifeq ($(GOOS),windows)
EXT := dll
else
EXT := so
endif

DIST := dist/$(GOOS)/$(GOARCH)
ARTIFACT := $(DIST)/$(PLUGIN_NAME)-v$(VERSION).$(EXT)

# INSTALL_DIR must match plugins.dir in config.yaml.
INSTALL_DIR ?= $(HOME)/.cli-proxy-api/plugins/$(GOOS)/$(GOARCH)

.PHONY: all build test vet install clean

all: test build

test:
	go test ./...

vet:
	go vet ./...

build:
	@mkdir -p $(DIST)
	CGO_ENABLED=1 go build -buildmode=c-shared -o $(ARTIFACT) .
	@echo "built $(ARTIFACT)"

install: build
	@mkdir -p $(INSTALL_DIR)
	cp $(ARTIFACT) $(INSTALL_DIR)/
	@echo "installed to $(INSTALL_DIR)/$(notdir $(ARTIFACT))"

clean:
	rm -rf dist
