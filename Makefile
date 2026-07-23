# serve - file server with QR-code transfer.
#
# The in-browser QR decoder is the qrstream package compiled to wasm
# and embedded into the binary. This Makefile tracks the wasm against
# its sources, so `make build`/`make run` always embed a current copy
# and it can never go stale.

# Go toolchain, in priority order: an explicit $GO, then go1.27rc2 when
# it is on PATH (jabcode's SIMD kernels target the Go 1.27 API), then go.
# Exported so wasm/generate.go picks the same toolchain for the wasm
# build and the matching wasm_exec.js it copies out.
GO ?= $(shell command -v go1.27rc2 >/dev/null 2>&1 && echo go1.27rc2 || echo go)
export GO

# jabcode's SIMD decode kernels sit behind the goexperiment.simd build
# tag (scalar fallback otherwise); GOEXPERIMENT=simd enables them in the
# host serve binary. Exported so every go invocation here agrees (the
# wasm build carries it inertly - SIMD is arch-gated off js/wasm).
GOEXPERIMENT ?= simd
export GOEXPERIMENT

UPX ?= upx

# Flags for every final/release Go build: strip the symbol table and
# DWARF debug info (-s -w) and drop absolute build paths for a
# reproducible binary (-trimpath). CGO_ENABLED=0 (set per recipe)
# links a static, dependency-free executable. The embedded wasm is
# built with the same -trimpath/-s/-w in wasm/generate.go.
GOFLAGS_RELEASE := -trimpath -ldflags='-s -w'

# Optional jabcode capabilities compiled into serve. The
# jabcode_non_iso_encode tag enables the experimental 16- and
# 32-color JAB sender modes (camera-marginal at 32; the untagged
# build stops at the 8-color ISO modes); jabcode_high_color is its
# decoder-side twin, needed by anything that reads high-color
# symbols (the future JAB wasm scanner module).
GOTAGS ?= jabcode_non_iso_encode,jabcode_high_color

WASM      := wasm/qrstream.wasm wasm/jabstream.wasm
WASM_EXEC := wasm/wasm_exec.js
# Each wasm target owns only its decoder and shim sources, so a QR-only
# generation does not rebuild JAB and vice versa.
QR_WASM_SRC := go.mod go.sum wasm/qrshim/main.go wasm/generate.go \
               $(filter-out %_test.go,$(wildcard qrstream/*.go))
JAB_WASM_SRC := go.mod go.sum wasm/jabshim/main.go wasm/generate.go \
                $(filter-out %_test.go,$(wildcard jabstream/*.go))

.PHONY: all build run test clean

all: build

# Used by run/test: each scanner module is regenerated only when its own
# shim or decoder sources change. Both commands copy the matching loader.
# wasm/generate.go uses the regular Go toolchain only - TinyGo is
# banned (its GC never runs finalizers, so syscall/js leaked every
# camera frame and crashed the iOS scanner; details in generate.go).
$(WASM_EXEC): go.mod go.sum wasm/generate.go
	$(GO) run wasm/generate.go loader

wasm/qrstream.wasm: $(QR_WASM_SRC)
	$(GO) run wasm/generate.go qr

wasm/jabstream.wasm: $(JAB_WASM_SRC)
	$(GO) run wasm/generate.go jab

# build always runs go generate (the //go:generate directive in
# main.go runs wasm/generate.go), so a release build can never embed
# stale wasm even when make's dependency tracking misses a change
# (e.g. vendored deps or a toolchain switch).
build:
	$(GO) generate
	CGO_ENABLED=0 $(GO) build $(GOFLAGS_RELEASE) -tags '$(GOTAGS)' -o serve .
	$(UPX) --ultra-brute serve

run: $(WASM) $(WASM_EXEC)
	$(GO) run -tags '$(GOTAGS)' .

test: $(WASM) $(WASM_EXEC)
	$(GO) vet -tags '$(GOTAGS)' ./...
	$(GO) test -tags '$(GOTAGS)' ./...

clean:
	rm -f -- serve $(WASM) $(WASM_EXEC)
