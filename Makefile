-include .env
export
SHELL := /bin/bash
GOROOT := /home/redacid/go
GOPATH := /home/redacid/gopath
BUILD_DIR := ./build
APP_NAME := kube-switch
OS := linux
ARCH := amd64
OSES := linux windows
ICON := pkg/resdata/resources/icon-green.png
PRJ_REPO := git@github.com:redacid/kube-switch.git


RELEASE_VERSION ?= 0.0.1
GO_RELEASER_VERSION := v2.7.0
#GO_RELEASER_VERSION := v2.8.1

# colors
GREEN = $(shell tput -Txterm setaf 2)
YELLOW = $(shell tput -Txterm setaf 3)
WHITE = $(shell tput -Txterm setaf 7)
RESET = $(shell tput -Txterm sgr0)
GRAY = $(shell tput -Txterm setaf 6)
TARGET_MAX_CHAR_NUM = 30

.EXPORT_ALL_VARIABLES:

all: help

mod-tidy:
	go mod tidy

run:
	go run ./

make_build_dir:
	mkdir -p $(BUILD_DIR)

build_linux: clean-workspace make_build_dir
	fyne build --release --output $(BUILD_DIR)/$(APP_NAME)_$(OS)_$(ARCH) --target $(OS) --metadata Details.Version=$(RELEASE_VERSION)
	chmod +x $(BUILD_DIR)/$(APP_NAME)_$(OS)_$(ARCH)

build_run_linux: build_linux
	$(BUILD_DIR)/$(APP_NAME)_$(OS)_$(ARCH)

release_linux: clean-workspace
	fyne release --name $(APP_NAME) --executable $(APP_NAME) -os $(OS) -icon $(ICON)

package_linux: clean-workspace make_build_dir
	fyne package --name $(APP_NAME) --release --executable $(APP_NAME) -os $(OS) -icon $(ICON)

fyne-cross-build-linux: install_fyne_cross_cmd
	fyne-cross linux -app-version $(RELEASE_VERSION) -arch amd64,386,arm,arm64 -icon $(ICON) -metadata Details.Version=$(RELEASE_VERSION) \
		-name $(APP_NAME) -release -debug

fyne-cross-build-windows: install_fyne_cross_cmd
	fyne-cross windows -app-version $(RELEASE_VERSION) -arch amd64,386 -icon $(ICON) -metadata Details.Version=$(RELEASE_VERSION) \
		-name $(APP_NAME) -debug

package_web:
	fyne package --release -os web

chrome_cors:
	/opt/google/chrome/chrome --user-data-dir="/tmp" --disable-web-security

.ONESHELL:
clean-workspace:
	rm *.tar.xz 2>/dev/null;
	rm -rf $(BUILD_DIR) 2>/dev/null;
	rm -rf ./fyne-cross 2> /dev/null;
	rm -rf ./dist 2>/dev/null;
	rm -rf ./tmp-pkg 2>/dev/null;
	rm fyne_metadata_init.go 2>/dev/null;
	rm -rf ./.zig-cache 2>/dev/null;
	rm -rf ./zig-out 2>/dev/null;
	rm -rf ./src 2>/dev/null;
	rm build.zig 2>/dev/null;
	rm build.zig.zon 2>/dev/null;
	exit 0;

install_linux_libs:
	sudo apt install freeglut3-dev gcc libgl1-mesa-dev xorg-dev libxkbcommon-dev

install_fyne_cmd:
	go install fyne.io/fyne/v2/cmd/fyne@latest

install_fyne_cross_cmd:
	go install github.com/fyne-io/fyne-cross@latest

git-release:
	gh release delete $(RELEASE_VERSION) --cleanup-tag -y --repo $(PRJ_REPO) 2>/dev/null;
	git tag -d $(RELEASE_VERSION) 2>/dev/null;
	gh release create $(RELEASE_VERSION) --generate-notes --notes "$(RELEASE_VERSION)" --repo $(PRJ_REPO)

git-upload-release-files: build_linux git-release
	gh release upload $(RELEASE_VERSION) $(BUILD_DIR)/$(APP_NAME)_$(OS)_$(ARCH) --repo $(PRJ_REPO)

git-publish:
	make clean-workspace
	make git-release
	make fyne-cross-build-linux
	make fyne-cross-build-windows
	make git-upload-release
	make clean-workspace

.ONESHELL:
git-upload-release:
	$(eval BIN_DIRS := $(shell ls ./fyne-cross/bin/))
	$(eval DIST_DIRS := $(shell ls ./fyne-cross/dist/))
	@for bin in $(BIN_DIRS); do
		if [[ $$bin == *"windows"* ]]; then
			mv "./fyne-cross/bin/"$$bin"/"$(APP_NAME)".exe" "./fyne-cross/bin/"$$bin"/"$(APP_NAME)_$$bin".exe" 2>/dev/null
			gh release upload $(RELEASE_VERSION) "./fyne-cross/bin/"$$bin"/"$(APP_NAME)_$$bin".exe" --repo $(PRJ_REPO)
		else
			mv "./fyne-cross/bin/"$$bin"/"$(APP_NAME) "./fyne-cross/bin/"$$bin"/"$(APP_NAME)_$$bin 2>/dev/null
			gh release upload $(RELEASE_VERSION) "./fyne-cross/bin/"$$bin"/"$(APP_NAME)_$$bin --repo $(PRJ_REPO)
		fi
	done
	@for dist in $(DIST_DIRS); do
		if [[ $$dist == *"windows"* ]]; then
			mv "./fyne-cross/dist/"$$dist"/"$(APP_NAME)".zip" "./fyne-cross/dist/"$$dist"/"$(APP_NAME)_$$dist".zip" 2>/dev/null
			gh release upload $(RELEASE_VERSION) "./fyne-cross/dist/"$$dist"/"$(APP_NAME)_$$dist".zip" --repo $(PRJ_REPO)
		else
			mv "./fyne-cross/dist/"$$dist"/"$(APP_NAME)".tar.xz" "./fyne-cross/dist/"$$dist"/"$(APP_NAME)_$$dist".tar.xz" 2>/dev/null
			gh release upload $(RELEASE_VERSION) "./fyne-cross/dist/"$$dist"/"$(APP_NAME)_$$dist".tar.xz" --repo $(PRJ_REPO)
		fi
	done

git-update:
	git pull && git fetch && git fetch --all

git-commit:
	git add -A
	git commit -m "Release create"
	git push

goreleaser: git-commit git-release
	goreleaser build --clean --single-target --verbose

goreleaser-build-static:
	docker run -t -e GOOS=linux -e GOARCH=amd64 -v $$PWD:/go/src/github.com/redacid/kube-switch -w /go/src/github.com/redacid/kube-switch goreleaser/goreleaser:$(GO_RELEASER_VERSION) build --clean --single-target --snapshot --verbose

#goreleaser-release: git-release git-update
#	#go clean -modcache
#	docker run -e GITHUB_TOKEN -e GIT_OWNER -it -v /var/run/docker.sock:/var/run/docker.sock -v $$PWD:/go/src/github.com/redacid/kube-switch -w /go/src/github.com/redacid/kube-switch goreleaser/goreleaser:$(GO_RELEASER_VERSION) release --clean --snapshot || exit 0;
#	docker container prune -f

goreleaser-release: clean-workspace #git-release git-update
	zig init
	goreleaser release --clean --snapshot || exit 0;


## Shows help. | Help
help:
	@echo ''
	@echo 'Usage:'
	@echo ''
	@echo '  ${YELLOW}make${RESET} ${GREEN}<target>${RESET}'
	@echo ''
	@echo 'Targets:'
	@awk '/^[a-zA-Z0-9\-_]+:/ { \
		helpMessage = match(lastLine, /^## (.*)/); \
		if (helpMessage) { \
		    if (index(lastLine, "|") != 0) { \
				stage = substr(lastLine, index(lastLine, "|") + 1); \
				printf "\n ${GRAY}%s: \n\n", stage;  \
			} \
			helpCommand = substr($$1, 0, index($$1, ":")-1); \
			helpMessage = substr(lastLine, RSTART + 3, RLENGTH); \
			if (index(lastLine, "|") != 0) { \
				helpMessage = substr(helpMessage, 0, index(helpMessage, "|")-1); \
			} \
			printf "  ${YELLOW}%-$(TARGET_MAX_CHAR_NUM)s${RESET} ${GREEN}%s${RESET}\n", helpCommand, helpMessage; \
		} \
	} \
	{ lastLine = $$0 }' $(MAKEFILE_LIST)
	@echo ''