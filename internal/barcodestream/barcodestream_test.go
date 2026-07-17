package barcodestream

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"testing"
)

func TestRejectsInvalidEncodingOptions(t *testing.T) {
	tests := []struct {
		name     string
		capacity int
		options  []Option
	}{
		{name: "capacity", capacity: headerLen},
		{name: "nil option", capacity: 84, options: []Option{nil}},
		{name: "redundancy", capacity: 84, options: []Option{WithFountain(0.5)}},
		{name: "nan redundancy", capacity: 84, options: []Option{WithFountain(math.NaN())}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Encode("file", nil, test.capacity, test.options...); err == nil {
				t.Fatal("Encode accepted invalid options")
			}
		})
	}
}

func TestStoredContainerRoundTrip(t *testing.T) {
	data := make([]byte, 3000)
	rand.New(rand.NewSource(1)).Read(data)
	stream, err := Encode("noise.bin", data, 84)
	if err != nil {
		t.Fatal(err)
	}
	if stream.flags&flagStore == 0 {
		t.Fatal("expected store flag for incompressible data")
	}

	collector := NewCollector()
	for frame := range stream.FrameBytes() {
		if _, err := collector.AddBytes(frame); err != nil {
			t.Fatal(err)
		}
	}
	name, decoded, err := collector.File()
	if err != nil {
		t.Fatal(err)
	}
	if name != "noise.bin" || !bytes.Equal(decoded, data) {
		t.Fatalf("round trip returned %q and %d bytes", name, len(decoded))
	}
}

func TestVersionOneWireFingerprint(t *testing.T) {
	data := make([]byte, 512)
	rand.New(rand.NewSource(23)).Read(data)
	tests := []struct {
		name       string
		storedName string
		data       []byte
		options    []Option
		fileID     uint32
		frames     int
		hash       string
	}{
		{
			name:       "sequential",
			storedName: "wire.bin",
			data:       data,
			fileID:     0xb3b858c5,
			frames:     8,
			hash:       "67eec9cd679a6090ae3e27475346da1d12b20cf28e798349ea8c6af9d2b656a9",
		},
		{
			name:       "compressed",
			storedName: "repeat.txt",
			data:       make([]byte, 4096),
			fileID:     0xdaba7ddb,
			frames:     1,
			hash:       "562527ca9d7b73107b1cba45053bc21e29e9a8fe4c8bf8ed9d58217abffacba0",
		},
		{
			name:       "fountain",
			storedName: "wire.bin",
			data:       data,
			options:    []Option{WithFountain(2)},
			fileID:     0xb3b858c5,
			frames:     16,
			hash:       "79294e9f0f2e2a38c377c9fe52a69939964536188b9b6d770e5e486a1aaf4884",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream, err := Encode(test.storedName, test.data, 84, test.options...)
			if err != nil {
				t.Fatal(err)
			}
			if stream.FileID() != test.fileID {
				t.Fatalf("file ID = %08x, want %08x", stream.FileID(), test.fileID)
			}
			if stream.NumFrames() != test.frames {
				t.Fatalf("frames = %d, want %d", stream.NumFrames(), test.frames)
			}
			if got := fingerprint(stream); got != test.hash {
				t.Fatalf("wire fingerprint = %s, want %s", got, test.hash)
			}
		})
	}
}

func fingerprint(stream *Stream) string {
	hash := sha256.New()
	for frame := range stream.FrameBytes() {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(frame)))
		hash.Write(size[:])
		hash.Write(frame)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func TestCollectorRejectsStreamOverCollectionLimit(t *testing.T) {
	collector := newCollector(8, 1024)
	raw := make([]byte, headerLen+5)
	header{fileID: 1, seq: 0, total: 2}.marshal(raw)
	if _, err := collector.AddBytes(raw); err == nil {
		t.Fatal("collector accepted frame geometry above its collection limit")
	}
}

func TestCollectorCollectionLimitIsGlobalAndForgetReleasesIt(t *testing.T) {
	collector := newCollector(10, 1024)
	frame := func(id uint32) []byte {
		raw := make([]byte, headerLen+6)
		header{fileID: id, seq: 0, total: 1}.marshal(raw)
		return raw
	}
	if _, err := collector.AddBytes(frame(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := collector.AddBytes(frame(2)); err == nil {
		t.Fatal("collector exceeded its global collection limit")
	}
	if _, ok := collector.streams[2]; ok {
		t.Fatal("collector retained a stream whose first frame was rejected")
	}
	collector.Forget(1)
	if _, err := collector.AddBytes(frame(2)); err != nil {
		t.Fatalf("collector did not release forgotten stream memory: %v", err)
	}
}

func TestCollectorBoundsActiveStreams(t *testing.T) {
	collector := newCollector(1024, 1024)
	collector.maxStreams = 1
	frame := func(id uint32) []byte {
		raw := make([]byte, headerLen+1)
		header{fileID: id, seq: 0, total: 2}.marshal(raw)
		return raw
	}
	if _, err := collector.AddBytes(frame(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := collector.AddBytes(frame(2)); err == nil {
		t.Fatal("collector exceeded its active stream limit")
	}
	collector.Forget(1)
	if _, err := collector.AddBytes(frame(2)); err != nil {
		t.Fatalf("collector did not release a forgotten stream slot: %v", err)
	}
}

func TestCollectorBoundsFrameCount(t *testing.T) {
	collector := newCollector(1024, 1024)
	collector.maxFrames = 1
	frame := func(seq uint16) []byte {
		raw := make([]byte, headerLen+1)
		header{fileID: 1, seq: seq, total: 2}.marshal(raw)
		return raw
	}
	if _, err := collector.AddBytes(frame(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := collector.AddBytes(frame(1)); err == nil {
		t.Fatal("collector exceeded its frame count limit")
	}
}

func TestCollectorBoundsDecompressedContainer(t *testing.T) {
	stream, err := Encode("large.txt", bytes.Repeat([]byte(`x`), 4096), 84)
	if err != nil {
		t.Fatal(err)
	}
	collector := newCollector(defaultMaxCollectedBytes, 1024)
	for raw := range stream.FrameBytes() {
		if _, err := collector.AddBytes(raw); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := collector.File(); err == nil {
		t.Fatal("collector decoded a container above its output limit")
	}
}
