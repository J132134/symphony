.PHONY: build install install-launchagents clean test

BINARY   = symphony
BIN_DIR  = bin
CMD_PATH = ./cmd/symphony

# Default install location; override with: make install INSTALL_DIR=~/bin
INSTALL_DIR = $(HOME)/.local/bin

build:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(BINARY) $(CMD_PATH)

install: build
	install -m 755 $(BIN_DIR)/$(BINARY) $(INSTALL_DIR)/$(BINARY)

install-launchagents:
	@mkdir -p $(HOME)/Library/LaunchAgents data
	sed -e 's|__HOME__|$(HOME)|g' -e 's|__REPO_DIR__|$(CURDIR)|g' \
		-e 's|__LINEAR_API_KEY__|$(LINEAR_API_KEY)|g' \
		scripts/com.symphony.daemon.plist > $(HOME)/Library/LaunchAgents/com.symphony.daemon.plist
	sed -e 's|__HOME__|$(HOME)|g' -e 's|__REPO_DIR__|$(CURDIR)|g' \
		scripts/com.symphony.menubar.plist > $(HOME)/Library/LaunchAgents/com.symphony.menubar.plist
	launchctl load $(HOME)/Library/LaunchAgents/com.symphony.daemon.plist 2>/dev/null || true
	launchctl load $(HOME)/Library/LaunchAgents/com.symphony.menubar.plist 2>/dev/null || true
	@echo "Installed. Check status: launchctl list | grep symphony"

clean:
	rm -rf $(BIN_DIR)

test:
	go test ./...

# Download modules and tidy go.sum
tidy:
	go mod tidy
