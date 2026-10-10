# The recipes use bash constructs and the build loops rely on parameter
# expansion, so do not let them run under dash.
SHELL := /bin/bash

GO            ?= go
MANIFEST_FILE ?= plugin.json
ASSETS_DIR    ?= assets

# The plugin id, the version and the platform -> executable mapping all come
# from the manifest, so it stays the single source of truth for them.
PLUGINNAME    = $(shell node -p "require('./$(MANIFEST_FILE)').id")
PLUGINVERSION = v$(shell node -p "require('./$(MANIFEST_FILE)').version")
PACKAGENAME   = mattermost-plugin-$(PLUGINNAME)-$(PLUGINVERSION)
PLATFORMS     = $(shell node -p "Object.keys(require('./$(MANIFEST_FILE)').server.executables).join(' ')")

# Platform that `make deploy` uploads to a development server.
PLATFORM ?= linux
ARCH     ?= amd64

BUILD_DIR  = dist/intermediate
BUNDLE_DIR = dist/$(PLUGINNAME)

# The version the plugin reports at runtime is injected at build time.
LDFLAGS = -ldflags="-X 'main.PluginVersion=$(PLUGINVERSION)'"

.PHONY: default build test test-server coverage dist buildserver package \
	check-style check-style-server fix-style fix-style-server vendor clean run stop deploy

default: check-style test dist

build: dist

check-style: check-style-server

check-style-server:
	if ! [ -x "$$(command -v golangci-lint)" ]; then \
		echo "golangci-lint is not installed. See https://golangci-lint.run/welcome/install/"; \
		exit 1; \
	fi
	echo Running golangci-lint
	golangci-lint run ./...

fix-style: fix-style-server

fix-style-server:
	if ! [ -x "$$(command -v golangci-lint)" ]; then \
		echo "golangci-lint is not installed. See https://golangci-lint.run/welcome/install/"; \
		exit 1; \
	fi
	echo Running golangci-lint --fix
	golangci-lint run --fix ./...

vendor:
	echo Downloading server dependencies
	$(GO) mod download

test-server: vendor
	echo Running server tests
	$(GO) test -race -v -coverprofile=coverage.txt ./...

test: test-server

coverage: test-server
	$(GO) tool cover -html=coverage.txt -o coverage.html

# Every platform listed in the manifest gets its own tarball, holding one server
# binary and a manifest narrowed to it, because a bundle that advertises a
# platform it does not carry is a bundle the server refuses to load.
dist: package
	echo Building plugin

buildserver:
	rm -rf $(BUILD_DIR)
	mkdir -p $(BUILD_DIR)
	@for platform in $(PLATFORMS); do \
		executable=$$(node -p "require('./$(MANIFEST_FILE)').server.executables['$$platform']") || exit 1; \
		echo "Building server for $$platform"; \
		CGO_ENABLED=0 GOOS=$${platform%-*} GOARCH=$${platform#*-} $(GO) build -trimpath $(GOFLAGS) \
			$(LDFLAGS) -o $(BUILD_DIR)/$$(basename $$executable) ./server || exit 1; \
	done

package: buildserver
	@for platform in $(PLATFORMS); do \
		executable=$$(node -p "require('./$(MANIFEST_FILE)').server.executables['$$platform']") || exit 1; \
		rm -rf $(BUNDLE_DIR); \
		mkdir -p $(BUNDLE_DIR)/server; \
		cp $(BUILD_DIR)/$$(basename $$executable) $(BUNDLE_DIR)/$$executable || exit 1; \
		cp -r $(ASSETS_DIR) $(BUNDLE_DIR)/ || exit 1; \
		node build/generate-manifest.js $(MANIFEST_FILE) $(BUNDLE_DIR)/plugin.json $$platform || exit 1; \
		(cd dist && tar -zcf $(PACKAGENAME)-$$platform.tar.gz $(PLUGINNAME)) || exit 1; \
		echo "Built dist/$(PACKAGENAME)-$$platform.tar.gz"; \
	done

clean:
	echo Cleaning plugin
	rm -rf dist coverage.txt coverage.html

run:
	echo Not yet implemented

stop:
	echo Not yet implemented

# deploy installs the plugin on a development server through the API. It needs
# MM_SERVICESETTINGS_SITEURL, MM_ADMIN_USERNAME and MM_ADMIN_PASSWORD to be set.
deploy:
	echo "Installing plugin via API"

	echo "Authenticating admin user..." && \
	TOKEN=`http --print h POST $(MM_SERVICESETTINGS_SITEURL)/api/v4/users/login login_id=$(MM_ADMIN_USERNAME) password=$(MM_ADMIN_PASSWORD) X-Requested-With:"XMLHttpRequest" | grep Token | cut -f2 -d' '` && \
	http GET $(MM_SERVICESETTINGS_SITEURL)/api/v4/users/me Authorization:"Bearer $$TOKEN" > /dev/null && \
	echo "Deleting existing plugin..." && \
	http DELETE $(MM_SERVICESETTINGS_SITEURL)/api/v4/plugins/$(PLUGINNAME) Authorization:"Bearer $$TOKEN" > /dev/null && \
	echo "Uploading plugin..." && \
	http --check-status --form POST $(MM_SERVICESETTINGS_SITEURL)/api/v4/plugins plugin@dist/$(PACKAGENAME)-$(PLATFORM)-$(ARCH).tar.gz Authorization:"Bearer $$TOKEN" > /dev/null && \
	echo "Enabling uploaded plugin..." && \
	http POST $(MM_SERVICESETTINGS_SITEURL)/api/v4/plugins/$(PLUGINNAME)/enable Authorization:"Bearer $$TOKEN" > /dev/null && \
	echo "Logging out admin user..." && \
	http POST $(MM_SERVICESETTINGS_SITEURL)/api/v4/users/logout Authorization:"Bearer $$TOKEN" > /dev/null && \
	echo "Plugin uploaded successfully"
