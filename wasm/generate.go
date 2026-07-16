//go:build ignore

// Builds the browser-side qrstream decoder (wasm/qrstream.wasm) and
// the matching JS loader (wasm/wasm_exec.js - the toolchain ships
// it). Run from the repo root via go generate.
//
// Deliberately uses the regular Go toolchain, NOT TinyGo. TinyGo's
// smaller binary is tempting, but its GC never runs finalizers
// (runtime.SetFinalizer is a no-op), and syscall/js releases JS
// handles only from a finalizer - so every camera frame passed into
// the wasm pinned its ~1.2 MB buffer in the worker forever
// (~12 MB/s leaked), crashing the iOS scanner mid-transfer
// (confirmed in the field 2026-06-12). Do not switch back to TinyGo
// unless its GC runs finalizers or the shim stops passing fresh JS
// objects per frame.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "wasm/generate:", err)
		os.Exit(1)
	}
}

func run() error {
	c := exec.Command(`go`, `build`, `-trimpath`, `-ldflags=-s -w`, `-o`, `wasm/qrstream.wasm`, `./wasm/shim`)
	c.Env = append(os.Environ(), `GOOS=js`, `GOARCH=wasm`)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return err
	}
	root, err := output(`go`, `env`, `GOROOT`)
	if err != nil {
		return err
	}
	js := filepath.Join(root, `lib`, `wasm`, `wasm_exec.js`)
	if _, err := os.Stat(js); err != nil {
		js = filepath.Join(root, `misc`, `wasm`, `wasm_exec.js`)
	}
	return done(`go`, js)
}

func output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

func done(toolchain, loaderJS string) error {
	src, err := os.ReadFile(loaderJS)
	if err != nil {
		return err
	}
	if err := os.WriteFile(`wasm/wasm_exec.js`, src, 0o644); err != nil {
		return err
	}
	fi, err := os.Stat(`wasm/qrstream.wasm`)
	if err != nil {
		return err
	}
	fmt.Printf("built with %s: %d bytes\n", toolchain, fi.Size())
	return nil
}
