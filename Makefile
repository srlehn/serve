# serve - file server with QR-code transfer.
#
# The in-browser QR decoder is the qrstream package compiled to wasm
# and embedded into the binary. This Makefile tracks the wasm against
# its sources, so `make build`/`make run` always embed a current copy
# and it can never go stale.

GO ?= go

# Flags for every final/release Go build: strip the symbol table and
# DWARF debug info (-s -w) and drop absolute build paths for a
# reproducible binary (-trimpath). CGO_ENABLED=0 (set per recipe)
# links a static, dependency-free executable. The embedded wasm is
# built with the same -trimpath/-s/-w in wasm/generate.go.
GOFLAGS_RELEASE := -trimpath -ldflags='-s -w'

WASM      := wasm/qrstream.wasm
WASM_EXEC := wasm/wasm_exec.js
# the wasm embeds the shim and the whole qrstream package (minus tests)
WASM_SRC := go.mod go.sum wasm/shim/main.go wasm/generate.go \
            $(filter-out %_test.go,$(wildcard qrstream/*.go))

.PHONY: all build run test clean

all: build

# Used by run/test: regenerated only when the shim or qrstream
# sources change. The grouped target records that one generator run
# writes both the wasm and its matching wasm_exec.js loader.
# wasm/generate.go uses the regular Go toolchain only - TinyGo is
# banned (its GC never runs finalizers, so syscall/js leaked every
# camera frame and crashed the iOS scanner; details in generate.go).
$(WASM) $(WASM_EXEC) &: $(WASM_SRC)
	$(GO) run wasm/generate.go

# build always runs go generate (the //go:generate directive in
# main.go runs wasm/generate.go), so a release build can never embed
# stale wasm even when make's dependency tracking misses a change
# (e.g. vendored deps or a toolchain switch).
build:
	$(GO) generate
	CGO_ENABLED=0 $(GO) build $(GOFLAGS_RELEASE) -o serve .

run: $(WASM_EXEC)
	$(GO) run .

test: $(WASM_EXEC)
	$(GO) vet ./...
	$(GO) test ./...

clean:
	rm -f -- serve $(WASM) $(WASM_EXEC)
