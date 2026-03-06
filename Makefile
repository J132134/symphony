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
	cp scripts/com.symphony.daemon.plist ~/Library/LaunchAgents/
	cp scripts/com.symphony.menubar.plist ~/Library/LaunchAgents/
	launchctl load ~/Library/LaunchAgents/com.symphony.daemon.plist 2>/dev/null || true
	launchctl load ~/Library/LaunchAgents/com.symphony.menubar.plist 2>/dev/null || true

clean:
	rm -rf $(BIN_DIR)

test:
	go test ./...

# Download modules and tidy go.sum
tidy:
	go mod tidy
