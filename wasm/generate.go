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
	mode := `all`
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	goBin := goTool()
	var modules []string
	switch mode {
	case `all`:
		if err := buildWasm(goBin, `wasm/qrstream.wasm`, `./wasm/qrshim`); err != nil {
			return err
		}
		if err := buildWasm(goBin, `wasm/jabstream.wasm`, `./wasm/jabshim`,
			`-tags=jabcode_non_iso_encode,jabcode_high_color`); err != nil {
			return err
		}
		modules = []string{`wasm/qrstream.wasm`, `wasm/jabstream.wasm`}
	case `qr`:
		if err := buildWasm(goBin, `wasm/qrstream.wasm`, `./wasm/qrshim`); err != nil {
			return err
		}
		modules = []string{`wasm/qrstream.wasm`}
	case `jab`:
		if err := buildWasm(goBin, `wasm/jabstream.wasm`, `./wasm/jabshim`,
			`-tags=jabcode_non_iso_encode,jabcode_high_color`); err != nil {
			return err
		}
		modules = []string{`wasm/jabstream.wasm`}
	case `loader`:
	default:
		return fmt.Errorf(`unknown wasm generation target %q`, mode)
	}
	root, err := output(goBin, `env`, `GOROOT`)
	if err != nil {
		return err
	}
	js := filepath.Join(root, `lib`, `wasm`, `wasm_exec.js`)
	if _, err := os.Stat(js); err != nil {
		js = filepath.Join(root, `misc`, `wasm`, `wasm_exec.js`)
	}
	return done(goBin, js, modules...)
}

// goTool picks the Go toolchain the same way the Makefile does: an
// explicit $GO wins, then go1.27rc2 when it is on PATH (jabcode's SIMD
// kernels target the Go 1.27 API), then plain go.
func goTool() string {
	if g := os.Getenv(`GO`); g != `` {
		return g
	}
	if p, err := exec.LookPath(`go1.27rc2`); err == nil {
		return p
	}
	return `go`
}

func buildWasm(goBin, out, pkg string, extraFlags ...string) error {
	args := append([]string{`build`, `-trimpath`, `-ldflags=-s -w`}, extraFlags...)
	args = append(args, `-o`, out, pkg)
	c := exec.Command(goBin, args...)
	c.Env = append(os.Environ(), `GOOS=js`, `GOARCH=wasm`)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

func output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

func done(toolchain, loaderJS string, modules ...string) error {
	src, err := os.ReadFile(loaderJS)
	if err != nil {
		return err
	}
	if err := os.WriteFile(`wasm/wasm_exec.js`, src, 0o644); err != nil {
		return err
	}
	for _, module := range modules {
		fi, err := os.Stat(module)
		if err != nil {
			return err
		}
		fmt.Printf("built %s with %s: %d bytes\n", module, toolchain, fi.Size())
	}
	return nil
}
