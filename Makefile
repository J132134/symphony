.PHONY: build install install-launchagents clean test

BINARY   = symphony
BIN_DIR  = bin
CMD_PATH = ./cmd/symphony
LOG_DIR  = $(HOME)/Library/Logs/Symphony
VERSION  ?= dev

# Default install location; override with: make install INSTALL_DIR=~/bin
INSTALL_DIR = $(HOME)/.local/bin

build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags "-X symphony/internal/version.version=$(VERSION)" -o $(BIN_DIR)/$(BINARY) $(CMD_PATH)

install: build
	install -m 755 $(BIN_DIR)/$(BINARY) $(INSTALL_DIR)/$(BINARY)


install-launchagents:
	@mkdir -p $(HOME)/Library/LaunchAgents $(LOG_DIR)
	sed -e 's|__HOME__|$(HOME)|g' -e 's|__LOG_DIR__|$(LOG_DIR)|g' \
		-e 's|__LINEAR_API_KEY__|$(LINEAR_API_KEY)|g' \
		scripts/com.symphony.daemon.plist > $(HOME)/Library/LaunchAgents/com.symphony.daemon.plist
	launchctl unload $(HOME)/Library/LaunchAgents/com.symphony.daemon.plist 2>/dev/null || true
	launchctl load $(HOME)/Library/LaunchAgents/com.symphony.daemon.plist 2>/dev/null || true
	@echo "Installed. Check status: launchctl list | grep symphony"

clean:
	rm -rf $(BIN_DIR)

test:
	go test ./...

# Download modules and tidy go.sum
tidy:
	go mod tidy
