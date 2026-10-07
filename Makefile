# juggler — workstreams for sway
#
#   make build       build/jug
#   make install     build + copy jug to ~/.local/bin and the sway include to ~/.config/sway
#   make test vet fmt
#   make poc         re-run the sway layout proof-of-concept (opens/closes windows!)
#   make poc-lot     same for the lot (parked workstreams) mechanics
#   make ui-shot     render the web UI headlessly (needs node + google-chrome, a running `jug serve`)
#   make service-install / service-enable / service-status / reinstall
#                    run `jug serve` (web UI + REST API) as a systemd --user service
#   make workspaces  one-time: rename the live sway workspaces to the N:name scheme
#                    (the config edit is manual, see README "Install")

SHELL := /bin/bash
BIN_DIR  := $(HOME)/.local/bin
SWAY_DIR := $(HOME)/.config/sway
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -X main.version=$(VERSION)

UNIT_DIR := $(HOME)/.config/systemd/user

.PHONY: build install test vet fmt check poc poc-lot ui-shot workspaces clean \
        service-install service-enable service-status reinstall

build:
	@mkdir -p build
	go build -ldflags '$(LDFLAGS)' -o build/jug ./cmd/jug

install: build
	install -d $(BIN_DIR) $(SWAY_DIR)
	install -m 0755 build/jug $(BIN_DIR)/jug
	install -m 0644 sway/juggler.conf $(SWAY_DIR)/juggler.conf
	@echo "installed $(BIN_DIR)/jug and $(SWAY_DIR)/juggler.conf"
	@grep -q 'juggler.conf' $(SWAY_DIR)/config || echo "NOTE: add to $(SWAY_DIR)/config:   include ~/.config/sway/juggler.conf"
	@echo "then: swaymsg reload"

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: fmt vet test build

poc:
	scripts/m0-sway-poc.sh

poc-lot:
	scripts/lot-poc.sh

poc-ensure:
	scripts/ensure-poc.sh

# Rename every live workspace "<name>" that lacks a number to "<N>:<name>",
# where N comes from the $wsN variables in the sway config. Run once, after
# editing the config to the N:name scheme and BEFORE `swaymsg reload`.
workspaces:
	scripts/rename-workspaces.sh

ui-shot:
	node scripts/ui-shot.mjs 'http://127.0.0.1:7474/?nolive' build/ui-list.png
	node scripts/ui-shot.mjs 'http://127.0.0.1:7474/?nolive' build/ui-detail.png 'document.querySelectorAll("#detail-pane fieldset").length' '#list tbody tr:first-child'
	@echo "screenshots: build/ui-list.png build/ui-detail.png"

# jug serve as a systemd --user service: web UI + REST API on 127.0.0.1:7474
# (config: listen). Supervision (restart on crash, relaunch on update) is
# systemd's job; `make reinstall` rebuilds and restarts it.
service-install: install
	install -d $(UNIT_DIR)
	install -m 0644 init/juggler.service $(UNIT_DIR)/juggler.service
	systemctl --user daemon-reload
	@echo "installed $(UNIT_DIR)/juggler.service; next: make service-enable"

service-enable:
	systemctl --user enable --now juggler
	@sleep 0.5; systemctl --user --no-pager status juggler | head -5

service-status:
	systemctl --user --no-pager status juggler | head -12

reinstall: install
	systemctl --user restart juggler
	@sleep 0.5; systemctl --user --no-pager status juggler | head -5

clean:
	rm -rf build
