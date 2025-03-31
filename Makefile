-include .env
export
SHELL := /bin/bash
#DEBUG := --debug
#VERBOSE := --verbose
BUILD_DIR := ./build
APP_NAME := kube-switch
OS := linux
ARCH := amd64
OSES := linux windows
ICON := pkg/resdata/resources/icon-green.png

RELEASE_VERSION ?= v0.0.1
#GO_RELEASER_VERSION := v2.7.0
GO_RELEASER_VERSION := v2.8.1

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

build_linux: clean make_build_dir
	fyne build --release --output $(BUILD_DIR)/$(APP_NAME)_$(OS)_$(ARCH) --target $(OS) --metadata Details.Version=$(RELEASE_VERSION)
	chmod +x $(BUILD_DIR)/$(APP_NAME)_$(OS)_$(ARCH)

build_run_linux: build_linux
	$(BUILD_DIR)/$(APP_NAME)_$(OS)_$(ARCH)

release_linux: clean
	fyne release --name $(APP_NAME) --executable $(APP_NAME) -os $(OS) -icon $(ICON)

package_linux: clean make_build_dir
	fyne package --name $(APP_NAME) --release --executable $(APP_NAME) -os $(OS) -icon $(ICON)

package_web:
	fyne package --release -os web

chrome_cors:
	/opt/google/chrome/chrome --user-data-dir="/tmp" --disable-web-security

clean:
	rm *.tar.xz 2> /dev/null || exit 0
	rm -rf $(BUILD_DIR) 2> /dev/null || exit 0

install_linux_libs:
	sudo apt install freeglut3-dev gcc libgl1-mesa-dev xorg-dev libxkbcommon-dev

install_fyne_cmd:
	go install fyne.io/fyne/v2/cmd/fyne@latest

install_fyne_cross_cmd:
	go install github.com/fyne-io/fyne-cross@latest

git-release: build_linux
	gh release delete $(RELEASE_VERSION) --cleanup-tag -y --repo git@github.com:redacid/kube-switch.git || exit 0;
	git tag -d $(RELEASE_VERSION) || exit 0;
	gh release create $(RELEASE_VERSION) --generate-notes --notes "$(RELEASE_VERSION)" --repo git@github.com:redacid/kube-switch.git

git-upload-release-files: git-release
	gh release upload $(RELEASE_VERSION) $(BUILD_DIR)/$(APP_NAME)_$(OS)_$(ARCH) --repo git@github.com:redacid/kube-switch.git

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

release: git-git-release
	docker run -e GITHUB_TOKEN -e GIT_OWNER -it -v /var/run/docker.sock:/var/run/docker.sock -v $$PWD:/go/src/github.com/redacid/kube-switch -w /go/src/github.com/redacid/kube-switch goreleaser/goreleaser:$(GO_RELEASER_VERSION) release --clean || exit 0;
	docker container prune -f

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