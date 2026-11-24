# greylistd-frontend build and release targets. `make help` lists them.
#
# VERSION comes from the latest v* tag (v1.2.3 -> 1.2.3, with -N-gSHA for
# commits after it); override it with `make VERSION=1.2.3 ...`. GOARCH picks
# the target architecture for dist/deb (default: the host's).

GO          ?= go
NAME        := greylistd-frontend
PKG         := ./cmd/greylistd-frontend
VERSION     ?= $(shell git describe --tags --match 'v*' 2>/dev/null | sed 's/^v//')
ifeq ($(VERSION),)
VERSION     := 0.0.0-dev
endif
GOOS        ?= linux
GOARCH      ?= $(shell $(GO) env GOARCH)
LDFLAGS     := -s -w -X main.version=$(VERSION)

DIST        := dist
BUILD_DIR   := $(DIST)/build/$(GOOS)_$(GOARCH)
BINARY      := $(BUILD_DIR)/$(NAME)
TARBALL_DIR := $(NAME)_$(VERSION)_$(GOOS)_$(GOARCH)

# Debian package (built with fpm, like opensmtpd-filter-greylistd). Debian
# versions sort "~" before anything, so 1.3.0-rc1 -> 1.3.0~rc1, and builds
# after a tag (1.2.3-4-gabc123) -> 1.2.3+git4.abc123.
DEB_PREFIX    := /usr/local/$(NAME)
DEB_VERSION   ?= $(shell echo '$(VERSION)' | sed -E 's/-([0-9]+)-g([0-9a-f]+)$$/+git\1.\2/; s/-/~/g')
DEB_ITERATION ?= 0
DEB_ARCH      := $(GOARCH)
DEB_FILE      := $(DIST)/$(NAME)_$(DEB_VERSION)-$(DEB_ITERATION)_$(DEB_ARCH).deb
DEB_ROOT      := $(DIST)/deb-root_$(DEB_ARCH)
FPM           ?= fpm

IMAGE         ?= $(NAME):$(VERSION)

.DEFAULT_GOAL := build

.PHONY: build
build: ## Build the static binary for GOOS/GOARCH into dist/build/
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath \
		-ldflags '$(LDFLAGS)' -o $(BINARY) $(PKG)

.PHONY: test
test: ## Run go vet and the tests with the race detector
	$(GO) vet ./...
	$(GO) test -race -count=1 ./...

.PHONY: lint
lint: ## Run golangci-lint (config in .golangci.yml)
	golangci-lint run

.PHONY: check
check: lint test ## Run lint and tests

.PHONY: dist
dist: build ## Release tarball: binary, READMEs and the systemd example
	rm -rf $(DIST)/$(TARBALL_DIR)
	install -D -m 0755 $(BINARY) $(DIST)/$(TARBALL_DIR)/$(NAME)
	install -m 0644 README.md README.rst $(DIST)/$(TARBALL_DIR)/
	install -D -m 0644 -t $(DIST)/$(TARBALL_DIR)/systemd deploy/systemd/$(NAME).service deploy/systemd/$(NAME).env
	tar -C $(DIST) -czf $(DIST)/$(TARBALL_DIR).tar.gz --owner=0 --group=0 $(TARBALL_DIR)
	rm -rf $(DIST)/$(TARBALL_DIR)
	@echo $(DIST)/$(TARBALL_DIR).tar.gz

.PHONY: deb
deb: build ## Debian package: binary in /usr/local/greylistd-frontend/bin, systemd example in .../example
	@command -v $(FPM) >/dev/null || { echo "fpm not found: gem install fpm" >&2; exit 1; }
	rm -rf $(DEB_ROOT) $(DEB_FILE)
	install -D -m 0755 $(BINARY) $(DEB_ROOT)$(DEB_PREFIX)/bin/$(NAME)
	install -D -m 0644 -t $(DEB_ROOT)$(DEB_PREFIX)/example deploy/systemd/$(NAME).service deploy/systemd/$(NAME).env
	install -D -m 0644 README.rst $(DEB_ROOT)$(DEB_PREFIX)/README.rst
	$(FPM) \
		-s dir \
		-t deb \
		-C $(DEB_ROOT) \
		-p $(DEB_FILE) \
		-n $(NAME) \
		-v "$(DEB_VERSION)" \
		--iteration "$(DEB_ITERATION)" \
		-a $(DEB_ARCH) \
		-m "Jonas Maurus" \
		--category net \
		-d "greylistd" \
		--description "Mobile-first web interface for greylistd." \
		--url "https://github.com/jdelic/greylistd-frontend" \
		usr
	rm -rf $(DEB_ROOT)

.PHONY: checksums
checksums: ## Write dist/SHA256SUMS for the tarballs and packages in dist/
	cd $(DIST) && sha256sum -- *.tar.gz *.deb > SHA256SUMS

.PHONY: docker
docker: ## Build the container image (IMAGE=name:tag)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

.PHONY: version
version: ## Print the version and Debian version that would be built
	@echo "version:     $(VERSION)"
	@echo "deb version: $(DEB_VERSION)-$(DEB_ITERATION)"

.PHONY: clean
clean: ## Remove dist/
	rm -rf $(DIST)

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-10s %s\n", $$1, $$2}'
