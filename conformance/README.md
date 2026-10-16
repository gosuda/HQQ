# Conformance vectors

Golden vectors for the HQQ shared-memory protocol (segment layout 3.0 `HQQ_STL3`, ring layout 2, protocol 1.1) and the tests that check the Go implementation against them. The normative text is in `spec/`; the vectors are its machine-checkable part. Any implementation, in any language, can read the same files.

## What is here

| Path | Content |
| --- | --- |
| `vectors/manifest.json` | Every vector group except golden sequences (schema: `Manifest` in `generate.go`). |
| `vectors/images/<page>/<case>.bin` | Superblock state images (G-08) for page families 4096 and 16384. A superblock image is the first page of the segment; a `full` image is the whole segment (rings formatted, buffers zero). |
| `vectors/rings/<page>/<case>.bin` | Whole-segment ring images (G-07), page families 4096 and 16384. |
| `vectors/sequences/<page>/` | Golden segment sequence (G-06): six normalized snapshots and `sequence.json`. Host page family only. |
| `layout.go`, `payload.go` | Shared constants (SPEC 2.1, 3, 7.1) and the deterministic payload (INTEROP_CLI section 5). Code, not vectors. |
| `generate.go` | The generator (`Generate`) and the manifest schema, documented per field. |

Vector groups in `manifest.json` (counts of the committed tree):

| Group | Field | Count | Covers |
| --- | --- | ---: | --- |
| G-02 packets | `packets` | 16 | the 9 `constants.json` packet vectors and 7 extra frames (OpConnCopy with Request|Meta, abort, response error, 1.1 negotiate and ack, OpCancel) |
| G-03 layouts | `layouts` | 16 | 12 `constants.json` geometries and 4 extra |
| G-04 geometry | `geometry` | 12 | valid and invalid buffer counts and sizes |
| G-05 names | `names` | 24 | SPEC 11.1 rules, macOS 30/31-byte names, Windows kernel-name vectors |
| G-11 error codes | `error_codes` | 11 | 7 codes of SPEC 12, ShuttingDown (1.1), unknown codes |
| payloads | `payloads` | 9 | INTEROP_CLI section 5: small payloads in hex, 1 KiB and 64 KiB by SHA-256 |
| G-08 superblocks | `superblocks` | 31 | 15 states per page family, and the recorded-page-65536 state (family 16384 only); each with the expected outcome of the Creator, Opener and InMemory attach paths |
| G-07 rings | `rings` | 12 | 6 ring states per page family (formatted, wrapped, wrapped at 2^64, empty at 2^64, full, half-published), with the expected Read and Write results |
| G-10 frames | `advanced_frames` | 44 | Advanced frame validation matrix (SPEC 10.5, PROTOCOL_1_1 sections 2 and 3), each row with frames, effect, deliveries or error |
| G-06 sequence | `sequences/<page>/` | 6 steps | Primary ready, Secondary joined, message in flight, consumed, both closed, re-initialized (epoch 2) |

## Manifest

The schema is the set of Go types in `generate.go` (`Manifest` and the types it refers to). Each field is documented in its type comment. Notes:

* Files are referenced by slash paths relative to `vectors/`.
* A 64-bit word that can exceed 2^53 (ring positions) is a `"0x..."` string (`Hex64`), as in `constants.json`.
* Byte payloads are lowercase hex. Packets are the 64-byte encoding of `internal/protocol`.
* The output is 2-space indented, with a trailing newline, and the field order is the struct order.
* `manifest.json` does not depend on the host. Golden sequences do, so they live in their own directory with `page_size` recorded.

## Normalization and page families

Images of a live Go run contain random owner tokens, a random creator token and wall-clock values. The generator replaces them so that the bytes are reproducible:

| Word (SPEC 3.1, 3.2) | Rule |
| --- | --- |
| `endpoints[i].token` | owner token -> `0x1111111111111111` (primary) or `0x2222222222222222` (secondary); 0 and 1 are kept |
| `endpoints[i].pid` | nonzero -> 0 |
| `endpoints[i].heartbeat` | nonzero -> 0 |
| `initToken` | nonzero -> `0x3333333333333333` |
| `initHeartbeat` | nonzero -> 0 |
| `creatorToken` | nonzero -> `0x4444444444444444` |

The rules are listed in `manifest.json` under `normalized`. Each golden step and each superblock case lists the words of its image that carry such values (`normalized`). Model images (superblock and ring cases) are written with the same values, so a port's own image of the same state can be compared byte for byte.

Page families. Superblock and ring images exist for 4096 and 16384. They are built by the layout model (`ComputeRegions`), which does not depend on the host, so both families are regenerated on any machine. The generator cross-checks the model against the Go implementation: it builds a Primary on caller memory with the host page size, normalizes it, and fails if it differs from the model image. Golden sequences need a Go run, so they exist for the host page family only. The recorded-page-65536 case is a 16384-family image whose superblock records page size 65536; it is never equal to the host page size on any host below 64 KiB.

## Regenerating and the byte-identical check

```text
go run ./cmd/hqq-interop gen-vectors --out conformance/vectors
```

The generator builds every file in memory first, so a failed run leaves the tree untouched. Then it removes only what it owns (`images/`, `rings/`, `sequences/<host page>/` and `manifest.json`), and writes. Nothing outside the output directory is touched, and other sequence families are kept.

Two tests enforce determinism:

* `TestGenerateDeterministic` generates twice into two temporary directories and requires identical trees.
* `TestCommittedVectorsCurrent` requires the committed tree to equal a fresh generation. A sequence family that the committed tree does not carry is not compared (it was produced on another host).

When the second test fails, regenerate and review the diff. A change that the generator or the spec does not explain is a bug to fix first.

## Running the tests

```text
go test ./conformance/... -count=1      # about 3 s on darwin/arm64 (16 KiB pages)
go test ./interop/... -count=1          # the two-process matrix (see below)
go run ./cmd/hqq-interop gen-vectors --out conformance/vectors
```

| Test | Groups |
| --- | --- |
| `TestConstantsMatchSpec` | G-01: `constants.json` against `layout.go` and the internal packages |
| `TestPacketVectors`, `TestPayloadVectors` | G-02 packets; payloads |
| `TestLayoutVectors` | G-03 |
| `TestGeometryLimits` | G-04 |
| `TestNameVectors` | G-05 (darwin and Windows vectors run only on their platform) |
| `TestGoldenSequence` | G-06 |
| `TestRingImages`, `TestRingAttachValidation` | G-07 |
| `TestSuperblockImages` | G-08 (cases of the other page family are skipped per case) |
| `TestPeerStateTransitions` | G-09 |
| `TestAdvancedFrameMatrix` | G-10 |
| `TestErrorCodeMapping` | G-11 |
| `TestGenerateDeterministic`, `TestCommittedVectorsCurrent` | determinism and the committed tree |

Two known disagreements between the documents are pinned by `TestConstantsMatchSpec` and reported rather than hidden: `constants.json` still describes protocol 1.0 (`protocol_minor` 0, encoded 256) while the code and `spec/ext/PROTOCOL_1_1_DRAFT.md` use 1.1; and `constants.json` `error_codes` lacks `ShuttingDown` (0x08, 1.1 draft).

## Using the vectors in a port

Levels follow `spec/PORTING.md` section 1.

| Level | Vector groups |
| --- | --- |
| L0 Layout | G-01 constants; G-02 packets and payloads; G-03 layouts; G-04 geometry; the field offsets of the superblock and ring images (G-07, G-08) |
| L1 Standard Opener, simple mode | G-08 rows of the Opener and InMemory-join cases; G-07 Read and Write results; G-09 peer states; G-11 error codes through Read; G-05 names on open |
| L2 Standard Creator and Primary | G-06 golden sequence (steps 1, 2, 5, 6, with their normalized words); G-08 rows of the Creator and InMemory-initialization cases; G-07 attach validation; G-05 names on create and removal |
| L3 Parking | no vector; the interop matrix (PORTING.md section 6.2, X-12) and the platform stress tests |
| L4 Advanced | G-10 every row; the 1.1 frames of G-02; ShuttingDown of G-11; the payloads of 1.1 messages |

A port reads `manifest.json`, then for each group either compares its own parsing or behavior with the expectations. For a superblock case, the port maps the image (the file is the superblock page; bytes beyond it are zero, or the full segment when `full` is set), performs the attach of the role, and compares the result and the write kind (`none`, `join`, `init`) with the expectation. For a frame row, the port feeds the frames to its receive path in order and compares the deliveries, the error kind, or the drop.

## Adding an implementation to the interop matrix

1. Implement the helper contract in `conformance/INTEROP_CLI.md`: flags, the result line, exit codes, barriers, the payload algorithm (check it with the payload vectors first) and the error-kind names.
2. Build an executable named `hqq-interop` (`hqq-interop.exe` on Windows). Check that `hqq-interop list --impl <label>` prints a catalogue; the driver runs a scenario on a pair only when both helpers list it. The catalogue of the Go helper is in `INTEROP_CLI.md` section 11.
3. Register the executable in `HQQ_INTEROP_IMPLS`, a comma-separated list of `label=path` pairs with labels matching `[a-z0-9]+`. For example `HQQ_INTEROP_IMPLS="go=,c=build-c/tools/hqq-interop"`. An empty path for `go` builds the Go helper from the module root. Relative paths are relative to the module root.
4. For soak sizes set `HQQ_INTEROP_LONG=1` (the weekly CI run does).
5. Add the build to `.github/workflows/interop.yml`: add a build step that produces the executable, then append `,<label>=<path>` to `HQQ_INTEROP_IMPLS` in the "Run interop matrix" step. The comment block at the top of that file lists the steps.
6. Run the matrix locally:

```text
HQQ_INTEROP_IMPLS="go=,c=build-c/tools/hqq-interop" go test ./interop/... -count=1 -timeout 30m
```
