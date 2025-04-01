-include .env
export
SHELL := /bin/bash
GOROOT := /home/redacid/go
GOPATH := /home/redacid/gopath
APP_NAME := kube-switch

ICON := internal/pkg/resdata/resources/icon-green.png
PRJ_REPO := git@github.com:redacid/kube-switch.git
RELEASE_VERSION ?= 0.0.1

# colors
GREEN = $(shell tput -Txterm setaf 2)
YELLOW = $(shell tput -Txterm setaf 3)
WHITE = $(shell tput -Txterm setaf 7)
RESET = $(shell tput -Txterm sgr0)
GRAY = $(shell tput -Txterm setaf 6)
TARGET_MAX_CHAR_NUM = 30

.EXPORT_ALL_VARIABLES:

all: help

## Build and Publish | Publish
git-publish:
	make clean-workspace
	#make git-release
	make fyne-cross-build-linux
	make fyne-cross-build-windows
	#make git-upload-release
	make clean-workspace


## Build Linux binaries | Build
fyne-cross-build-linux: install_fyne_cross_cmd
	fyne-cross linux -debug -app-version $(RELEASE_VERSION) -arch amd64,386,arm,arm64 -icon $(ICON) -metadata Details.Version=$(RELEASE_VERSION) \
		-name $(APP_NAME)
## Build Windows binaries
fyne-cross-build-windows: install_fyne_cross_cmd
	fyne-cross windows -app-version $(RELEASE_VERSION) -arch amd64,386 -icon $(ICON) -metadata Details.Version=$(RELEASE_VERSION) \
		-name $(APP_NAME) -debug

## Create release on git | Git
git-release:
	gh release delete $(RELEASE_VERSION) --cleanup-tag -y --repo $(PRJ_REPO) 2>/dev/null;
	git tag -d $(RELEASE_VERSION) 2>/dev/null;
	gh release create $(RELEASE_VERSION) --generate-notes --notes "$(RELEASE_VERSION)" --repo $(PRJ_REPO)

.ONESHELL:
## Upload release binaries
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

## Update project from git
git-update:
	git pull && git fetch && git fetch --all

## Install Dependency | Install
install_linux_libs:
	sudo apt install freeglut3-dev gcc libgl1-mesa-dev xorg-dev libxkbcommon-dev
## Install fyne compiler
install_fyne_cmd:
	go install fyne.io/fyne/v2/cmd/fyne@latest
## Install fyne cross-platform compiler
install_fyne_cross_cmd:
	go install github.com/fyne-io/fyne-cross@latest

.ONESHELL:
## Clear workspace | Clean
clean-workspace:
	rm *.tar.xz 2>/dev/null ;
	rm -rf $(BUILD_DIR) 2>/dev/null;
	rm -rf ./fyne-cross 2> /dev/null;
	rm -rf ./dist 2>/dev/null;
	rm -rf ./tmp-pkg 2>/dev/null;
	rm fyne_metadata_init.go 2>/dev/null;
	exit 0;

## Get golang modules | Tools
go-mod-tidy:
	go mod tidy

## Run project
go-run:
	go run ./...

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