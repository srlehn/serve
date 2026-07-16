// Command qrstream encodes a file into a looping QR-code GIF (or serves
// it as a motion video stream) and decodes such streams back from GIFs
// or photo/still images.
//
//	qrstream encode [-ver 25] [-level Q] [-px 8] [-fps 2] [-fountain] [-redundancy 2] [-name n] [-o out.gif] [-serve addr] [file|-]
//	qrstream decode [-o dir|-] [input.gif|img.jpg|img.png|-] ...
//
// encode reads stdin when the input is "-" or absent (the stored file
// name then defaults to "stdin"; override with -name) and writes the
// GIF to stdout when reading stdin or with -o -. decode reads stdin
// for "-" or no arguments, writes the decoded file contents to stdout
// by default, and writes to a directory under the stored name with
// -o dir. Inputs are sniffed by content, not extension, so pipelines
// like
//
//	tar cz . | qrstream encode - | qrstream decode - > out.tar.gz
//
// work.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	qrstream "github.com/srlehn/serve/qrstream"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "encode":
		encode(os.Args[2:])
	case "decode":
		decode(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: qrstream encode|decode [flags] [file|-] ...")
	os.Exit(2)
}

func encode(args []string) {
	fs := flag.NewFlagSet("encode", flag.ExitOnError)
	ver := fs.Int("ver", 25, "QR version 1..40")
	level := fs.String("level", "Q", "EC level L|M|Q|H")
	px := fs.Int("px", 8, "pixels per module")
	fps := fs.Float64("fps", 2, "frames per second")
	name := fs.String("name", "", "stored file name (default: input base name, or \"stdin\")")
	fountainMode := fs.Bool("fountain", false, "rateless LT fountain coding: any sufficient frame subset decodes")
	redundancy := fs.Float64("redundancy", 2, "fountain loop length as multiple of the source-block count")
	out := fs.String("o", "", "output GIF; - for stdout (default <file>.qr.gif, stdout for stdin input)")
	serve := fs.String("serve", "", "serve motion stream on this address instead (e.g. :8080)")
	fs.Parse(args)
	if fs.NArg() > 1 {
		usage()
	}
	path := "-"
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}

	var data []byte
	var err error
	stored := *name
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
		check(err)
		if stored == "" {
			stored = "stdin"
		}
	} else {
		data, err = os.ReadFile(path)
		check(err)
		if stored == "" {
			stored = filepath.Base(path)
		}
	}

	lvl, ok := map[string]qrstream.Level{"L": qrstream.L, "M": qrstream.M, "Q": qrstream.Q, "H": qrstream.H}[*level]
	if !ok {
		check(fmt.Errorf("bad level %q", *level))
	}
	st, err := qrstream.Encode(stored, data, &qrstream.Options{
		Version: *ver, Level: lvl, ModulePx: *px, FPS: *fps,
		Fountain: *fountainMode, Redundancy: *redundancy,
	})
	check(err)
	fmt.Fprintf(os.Stderr, "%s: %d bytes -> %d frames (v%d-%s), stream %08x\n",
		stored, len(data), st.NumFrames(), *ver, *level, st.FileID())

	if *serve != "" {
		fmt.Fprintf(os.Stderr, "serving motion stream loop on %s\n", serveURL(*serve))
		check(http.ListenAndServe(*serve, st))
	}
	g, err := buildGIF(st, *fps)
	check(err)
	dst := *out
	if dst == "" {
		if path == "-" {
			dst = "-"
		} else {
			dst = path + ".qr.gif"
		}
	}
	if dst == "-" {
		check(gif.EncodeAll(os.Stdout, g))
		return
	}
	f, err := os.Create(dst)
	check(err)
	check(gif.EncodeAll(f, g))
	check(f.Close())
	fmt.Fprintf(os.Stderr, "wrote %s\n", dst)
}

// buildGIF assembles the frame loop into an endlessly repeating
// animated GIF.
func buildGIF(st *qrstream.Stream, fps float64) (*gif.GIF, error) {
	delay := int(100/fps + 0.5) // GIF delays tick in 1/100 s
	g := &gif.GIF{LoopCount: 0}
	palette := color.Palette{color.White, color.Black}
	for img, err := range st.Frames() {
		if err != nil {
			return nil, err
		}
		pal := image.NewPaletted(img.Bounds(), palette)
		draw.Draw(pal, pal.Bounds(), img, img.Bounds().Min, draw.Src)
		g.Image = append(g.Image, pal)
		g.Delay = append(g.Delay, delay)
	}
	return g, nil
}

// serveURL turns a listen address into a clickable URL: a missing or
// wildcard host becomes localhost.
func serveURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + "/"
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

func decode(args []string) {
	fs := flag.NewFlagSet("decode", flag.ExitOnError)
	out := fs.String("o", "-", "output directory, or - for stdout")
	fs.Parse(args)
	paths := fs.Args()
	if len(paths) == 0 {
		paths = []string{"-"}
	}
	c := qrstream.NewCollector()

	// symbol extraction is the CPU-bound step; fan it out one worker
	// per core, stop feeding as soon as any stream completes
	type job struct {
		path string
		img  image.Image
	}
	var (
		mu   sync.Mutex
		last qrstream.Progress
		wg   sync.WaitGroup
		once sync.Once
	)
	jobs := make(chan job)
	done := make(chan struct{})
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for j := range jobs {
				prog, err := c.Add(j.img)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", j.path, err)
					continue
				}
				mu.Lock()
				if !last.Done && (prog.Done || prog.Have > last.Have) {
					last = prog
				}
				mu.Unlock()
				if prog.Done {
					once.Do(func() { close(done) })
					return
				}
			}
		})
	}
feed:
	for _, path := range paths {
		for _, img := range frames(path) {
			select {
			case jobs <- job{path, img}:
			case <-done:
				break feed
			}
		}
	}
	close(jobs)
	wg.Wait()
	if !last.Done {
		check(fmt.Errorf("incomplete: %d/%d frames, missing %v", last.Have, last.Total, c.Missing(last.FileID)))
	}
	name, data, err := c.File()
	check(err)
	if *out == "-" {
		_, err = os.Stdout.Write(data)
		check(err)
		fmt.Fprintf(os.Stderr, "%s (%d bytes) -> stdout\n", name, len(data))
		return
	}
	dst := filepath.Join(*out, filepath.Base(name))
	check(os.WriteFile(dst, data, 0o644))
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", dst, len(data))
}

// frames yields all images contained in path ("-" for stdin); the
// format is sniffed by content, and GIFs contribute every frame.
func frames(path string) []image.Image {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	check(err)
	if bytes.HasPrefix(data, []byte("GIF8")) {
		g, err := gif.DecodeAll(bytes.NewReader(data))
		check(err)
		out := make([]image.Image, len(g.Image))
		for i, im := range g.Image {
			out[i] = im
		}
		return out
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	check(err)
	return []image.Image{img}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "qrstream:", err)
		os.Exit(1)
	}
}
