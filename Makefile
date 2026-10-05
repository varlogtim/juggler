# juggler — workstreams for sway
#
#   make build       build/jug
#   make install     build + copy jug to ~/.local/bin and the sway include to ~/.config/sway
#   make test vet fmt
#   make poc         re-run the sway layout proof-of-concept (opens/closes windows!)
#   make poc-lot     same for the lot (parked workstreams) mechanics
#   make workspaces  one-time: rename the live sway workspaces to the N:name scheme
#                    (the config edit is manual, see README "Install")

SHELL := /bin/bash
BIN_DIR  := $(HOME)/.local/bin
SWAY_DIR := $(HOME)/.config/sway
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -X main.version=$(VERSION)

.PHONY: build install test vet fmt check poc poc-lot workspaces clean

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

# Rename every live workspace "<name>" that lacks a number to "<N>:<name>",
# where N comes from the $wsN variables in the sway config. Run once, after
# editing the config to the N:name scheme and BEFORE `swaymsg reload`.
workspaces:
	scripts/rename-workspaces.sh

clean:
	rm -rf build
