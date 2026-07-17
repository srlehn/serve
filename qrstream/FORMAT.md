# qrstream wire format, version 1

Normative description; enough to write an independent implementation.

## Symbol

Each frame is one QR symbol (ISO/IEC 18004), any version 1-40, any EC
level. All frames of a stream use the same version and EC level (keeps
finder-pattern geometry static for a scanning camera). The frame bytes
are carried in 8-bit byte mode; the symbol is filled to its exact
byte-mode data capacity except for the final frame, which may be
shorter. Standard 4-module quiet zone.

## Frame

```text
offset length field
0      2      magic: 0x71 0x53 ("qS")
2      1      high nibble: format version = 1
              low nibble: flags
3      4      fileID, big endian: CRC-32C (Castagnoli) of the
              complete payload (the possibly-compressed container)
7      2      seq, big endian: 0-based frame index
9      2      total, big endian: frame count; 0 is invalid
11     ...    payload chunk: payload[seq*per : (seq+1)*per] where
              per = capacity(version, level) - 11
```

Flags:

- Bit 0 enables fountain mode, where seq/total are reinterpreted as
  block seed and source-block count. See "Fountain mode".
- Bit 1 stores the container without compression.
- Bits 2-3 are reserved and must be zero.

A decoder must reject frames whose magic or format version differ,
and treat fileID as the stream key: frames with different fileIDs
belong to different streams; frames with the same fileID but
conflicting total or flags are corrupt.

## Container

The payload, after concatenating chunks in seq order and verifying
CRC-32C(payload) == fileID, is:

```text
zstd(container)          if flags bit 1 clear
container                if flags bit 1 set
```

with

```text
container = uvarint(len(name)) ‖ name ‖ file bytes
```

`uvarint` is Go's `encoding/binary` unsigned varint. `name` is the
file name as UTF-8, no path. `zstd()` is a single zstd frame (RFC
8878) at the encoder's highest compression level, without the
optional frame checksum - whole-file integrity is the fileID. The
encoder sets the store flag when compression does not shrink the
container ("store if bigger").

End of stream: there is no terminator frame; `total` in every frame is
the end signal. Collection is complete when all seq 0..total-1 are
held (sequential mode) or all source blocks are recovered (fountain
mode). Whole-file integrity is the fileID check; per-frame integrity
is the QR symbol's own Reed-Solomon coding (a frame either decodes
correctly or not at all).

Determinism: no timestamps or randomness anywhere; identical
name+data+options produce identical frames and fileID.

## Fountain mode (flags bit 0)

Rateless Luby Transform coding: every frame is the XOR of a
seed-determined subset of K fixed source blocks, so a receiver
reconstructs from *any* sufficiently large frame subset (typically a
few percent more than K) - no specific frame is ever "the missing
one".

Frame reinterpretation: `seq` carries the **block seed**, `total`
carries **K**, the source-block count. The `seq < total` rule of
sequential mode does not apply; seeds are arbitrary uint16 values and
duplicates are free. Every chunk has exactly `per` bytes (frames
always fill the symbol).

Message construction: because the padding to whole blocks is not
recoverable from K alone, the payload carries its own length:

```text
message = uvarint(len(payload)) ‖ payload ‖ zero padding to K*per
```

K is the smallest count such that `K*per >= len(uvarint ‖ payload)`.
The K source blocks are `message[i*per : (i+1)*per]`. `fileID` remains
CRC-32C of the (unpadded) payload.

Block composition for a seed:

1. Seed an MT19937 PRNG (32-bit variant of Matsumoto/Nishimura with
   the improved 1812433253 initializer; a 64-bit seed is folded by
   XORing its halves - a no-op for uint16 seeds).
1. Draw the degree d: take a uniform r in [0,1) and find the smallest
   d with `idealSolitonCDF(K)[d] >= r`, where the one-based CDF is
   cdf[1] = 1/K, cdf[i] = cdf[i-1] + 1/(i*(i-1)).
1. Draw d distinct block indices uniformly from [0,K) by rejection
   sampling; if d >= K, all indices are used and the PRNG is not
   consulted.
1. The chunk is the XOR of the selected source blocks.

The uniform-value derivations (Float64, Intn) follow Go's
math/rand(v1) semantics on the raw MT19937 output;
`internal/barcodestream/fountain.go` is the reference implementation,
bit-compatible with google/gofountain's LubyCodec so the exact
bit-level derivation is pinned by two independent codebases.

The encoder emits one loop of ceil(K * redundancy) frames with seeds
0,1,2,… (redundancy 2 by default, the factor txqr validated on real
cameras); this is an encoder choice, not part of the format - a
decoder accepts any seeds in any order. Determinism is preserved: the
seed fully determines each frame.

## Defaults and rationale

Default symbol: version 25, EC level Q (715 data bytes, 25%
correction). Camera robustness degrades sharply above ~v30 because
pixels per module shrink. Header overhead is 11 bytes/frame ≈ 1.5% at
v25-Q.
