PREFIX ?= $(HOME)/.local
CONFIG_DIR ?= $(HOME)/.config/fbsync
UNIT_DIR ?= $(HOME)/.config/systemd/user
APPS_DIR ?= $(HOME)/.local/share/applications
AUTOSTART_DIR ?= $(HOME)/.config/autostart
BIN := $(PREFIX)/bin/fbsync

.PHONY: build install install-service uninstall

build:
	go build -trimpath -ldflags='-s -w' -o fbsync .

# Tray app: binary, app-menu entry, start at login.
install: build
	install -Dm755 fbsync $(BIN)
	install -d $(APPS_DIR) $(AUTOSTART_DIR)
	sed 's|@BIN@|$(BIN)|' contrib/fbsync.desktop > $(APPS_DIR)/fbsync.desktop
	sed 's|@BIN@|$(BIN)|' contrib/fbsync.desktop > $(AUTOSTART_DIR)/fbsync.desktop
	@if [ ! -e $(CONFIG_DIR)/config.yaml ]; then \
		install -Dm600 config.example.yaml $(CONFIG_DIR)/config.yaml; \
		echo "Created $(CONFIG_DIR)/config.yaml - edit it (or use the tray's Edit config)"; \
	fi

# Headless alternative to the tray (servers, no desktop). Use one or the other.
install-service: build
	install -Dm755 fbsync $(BIN)
	install -Dm644 contrib/fbsync.service $(UNIT_DIR)/fbsync.service
	systemctl --user daemon-reload

uninstall:
	-systemctl --user disable --now fbsync.service
	rm -f $(BIN) $(UNIT_DIR)/fbsync.service $(APPS_DIR)/fbsync.desktop $(AUTOSTART_DIR)/fbsync.desktop
	-systemctl --user daemon-reload
