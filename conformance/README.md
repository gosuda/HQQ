# Conformance Vectors

This directory contains the machine-checkable golden vectors for the HQQ shared-memory protocol (segment layout 3.0 `HQQ_STL3`, ring layout 2, and protocol version 1.1) along with the test suite that verifies the Go implementation against them. While [`spec/`](../spec/SPEC.md) provides the normative prose specification, `conformance/vectors/` provides the exact byte layouts, state transitions, and frame sequences that every implementation in any language must satisfy.

## Directory Layout and Vector Groups

The files in `conformance/` separate the deterministic generator code from the committed binary and JSON vector artifacts:

| Path | Content |
| --- | --- |
| `vectors/manifest.json` | Master JSON catalogue containing every vector group except the multi-step golden sequences (schema defined by `Manifest` in [`generate.go`](generate.go)). |
| `vectors/images/<page>/<case>.bin` | Superblock state images (`G-08`) for page-size families `4096` and `16384`. Standard superblock images contain the first page of the segment; `full` images contain the entire segment with formatted rings and zeroed payload slots. |
| `vectors/rings/<page>/<case>.bin` | Whole-segment ring state images (`G-07`) for page-size families `4096` and `16384`. |
| `vectors/sequences/<page>/` | Multi-step golden segment lifecycle sequence (`G-06`): six normalized full-segment snapshots plus `sequence.json` for the host page-size family. |
| [`layout.go`](layout.go), [`payload.go`](payload.go) | Shared layout constants (`SPEC.md` sections 2.1, 3, and 7.1) and the deterministic SplitMix64 + CRC-32 payload generator ([`INTEROP_CLI.md`](INTEROP_CLI.md) section 5). |
| [`generate.go`](generate.go) | The deterministic vector generator (`Generate`) and the Go struct definitions that define the `manifest.json` schema. |

The committed `vectors/manifest.json` and `vectors/sequences/` directories contain the following vector counts across ten test groups:

| Group | Manifest Field | Count | Coverage |
| --- | --- | ---: | --- |
| `G-02` packets | `packets` | 16 | The 9 base packet vectors from `constants.json` plus 7 additional frames (`OpConnCopy` with `Request\|Meta`, response abort, response error, Protocol 1.1 `OpProtoNegotiate` and `OpProtoAck`, and `OpCancel`). |
| `G-03` layouts | `layouts` | 16 | The 12 segment geometries from `constants.json` plus 4 extended geometries. |
| `G-04` geometry | `geometry` | 12 | Valid and invalid `bufferCount` and `bufferSize` combinations. |
| `G-05` names | `names` | 24 | Segment naming rules (`SPEC.md` section 11.1), macOS 30-byte and 31-byte POSIX shm names, and Windows `Local\hqq_v3_` kernel object paths. |
| `G-11` error codes | `error_codes` | 11 | The 7 base wire error codes (`SPEC.md` section 12), `ShuttingDown` (`0x08` in Protocol 1.1), and unknown code fallback (`ErrProtocol`). |
| payloads | `payloads` | 9 | Deterministic payload vectors ([`INTEROP_CLI.md`](INTEROP_CLI.md) section 5): small payloads encoded in hex, and 1 KiB and 64 KiB payloads verified by SHA-256 digest. |
| `G-08` superblocks | `superblocks` | 31 | 15 superblock states per page family (`4096` and `16384`) plus the `recorded-page-65536` mismatch case in family `16384`, each specifying the expected attach outcome for Creator, Opener, and InMemory callers. |
| `G-07` rings | `rings` | 12 | 6 ring states per page family (freshly formatted, wrapped around the ring, wrapped across `2^64`, empty at `2^64`, full, and half-published), with expected `Read` and `Write` outcomes. |
| `G-10` frames | `advanced_frames` | 44 | Advanced frame validation matrix (`SPEC.md` section 10.5 and `PROTOCOL_1_1_DRAFT.md` sections 2 and 3), mapping input frame sequences to state transitions, message deliveries, or protocol errors. |
| `G-06` sequence | `sequences/<page>/` | 6 steps | Full-segment snapshots across a complete lifecycle: Primary ready, Secondary joined, message in flight, message consumed, both endpoints closed, and segment re-initialized at epoch 2. |

## Manifest Schema Conventions

The JSON schema of `vectors/manifest.json` is defined directly by the Go struct types in [`generate.go`](generate.go) (`Manifest` and its nested element types):

- **Relative paths**: Binary image files are referenced by forward-slash paths relative to `conformance/vectors/`.
- **64-bit unsigned integers**: Any 64-bit word that can exceed `2^53 - 1` (such as ring sequence counters and tokens) is serialized as a `"0x..."` hexadecimal string (`Hex64`), matching `spec/constants.json`.
- **Byte slices and packets**: Raw byte payloads are encoded as lowercase hexadecimal strings. Packet vectors are 64-byte wire headers encoded according to `internal/protocol`.
- **Canonical formatting**: `manifest.json` is formatted with 2-space indentation, struct-declaration field ordering, and a single trailing newline.
- **Host independence**: `manifest.json`, `images/`, and `rings/` are completely independent of the host OS and CPU architecture. Only `sequences/<page>/` depends on the host page size, so each page family resides in its own subdirectory with `page_size` recorded in `sequence.json`.

## Snapshot Normalization and Page Families

Live shared-memory segments created by the Go runtime contain cryptographic random owner tokens, random creator tokens, process IDs, and monotonic heartbeat timestamps. To make binary segment snapshots byte-for-byte reproducible across runs, the generator normalizes those fields using fixed sentinel constants:

| Superblock Word (`SPEC.md` 3.1, 3.2) | Normalization Rule |
| --- | --- |
| `endpoints[i].token` | Live owner token replaced with `0x1111111111111111` (Primary) or `0x2222222222222222` (Secondary); `0` (empty) and `1` (claiming) are preserved unchanged. |
| `endpoints[i].pid` | Any non-zero PID is replaced with `0`. |
| `endpoints[i].heartbeat` | Any non-zero timestamp is replaced with `0`. |
| `initToken` | Any non-zero token is replaced with `0x3333333333333333`. |
| `initHeartbeat` | Any non-zero timestamp is replaced with `0`. |
| `creatorToken` | Any non-zero token is replaced with `0x4444444444444444`. |

These rules are recorded in `manifest.json` under the top-level `normalized` field, and each golden sequence step and superblock case lists the exact word offsets normalized in its image. Synthetic model images (`images/` and `rings/`) are constructed directly with these normalized constants so a port's own in-memory segment can be normalized and compared byte for byte.

Both `4096`-byte and `16384`-byte page families are generated for all `images/` and `rings/` cases on any machine via the pure-computation layout model (`ComputeRegions`). During generation, `Generate` cross-checks the layout model against the live Go implementation by initializing a real Primary endpoint in caller-owned memory at the host page size, normalizing the resulting segment, and verifying that it is byte-identical to the model image. The `recorded-page-65536` test case is stored under the `16384` family with `65536` written into the superblock's `pageSize` field so it triggers a page-size mismatch on both 4 KiB and 16 KiB hosts.

## Regenerating the Vectors

To regenerate the conformance vectors from the reference generator, run:

```bash
go run ./cmd/hqq-interop gen-vectors --out conformance/vectors
```

The generator builds every file in memory before touching the filesystem, ensuring that a failed run leaves the working tree unmodified. Once generation succeeds, it replaces `images/`, `rings/`, `sequences/<host-page-size>/`, and `manifest.json` while preserving `sequences/` directories belonging to other host page sizes.

Two automated tests guard vector reproducibility:

- `TestGenerateDeterministic`: Runs `Generate` twice into separate temporary directories and verifies that every output file is byte-identical.
- `TestCommittedVectorsCurrent`: Runs `Generate` in memory and verifies that the committed files under `conformance/vectors/` match the fresh output byte for byte.

## Running the Conformance Tests

Execute the conformance test suite and the two-process interop matrix from the repository root:

```bash
go test ./conformance/... -count=1      # completes in ~3s on darwin/arm64 (16 KiB pages)
go test ./interop/... -count=1          # runs the two-process interop matrix
go run ./cmd/hqq-interop gen-vectors --out conformance/vectors
```

Each test function in `conformance/` validates a specific vector group:

| Test Function | Vector Groups Verified |
| --- | --- |
| `TestConstantsMatchSpec` | `G-01`: Cross-checks `spec/constants.json` against `layout.go` and `internal/protocol`. |
| `TestPacketVectors`, `TestPayloadVectors` | `G-02` wire packets and deterministic SplitMix64/CRC-32 payloads. |
| `TestLayoutVectors` | `G-03` segment region offsets and total segment sizes. |
| `TestGeometryLimits` | `G-04` valid and invalid buffer counts and sizes. |
| `TestNameVectors` | `G-05` segment name validation (Darwin and Windows kernel-path vectors run on their respective OS). |
| `TestGoldenSequence` | `G-06` six-step segment lifecycle snapshots. |
| `TestRingImages`, `TestRingAttachValidation` | `G-07` ring buffer reads, writes, wrap-around at `2^64`, and attach validation. |
| `TestSuperblockImages` | `G-08` superblock state machine across Creator, Opener, and InMemory attach paths. |
| `TestPeerStateTransitions` | `G-09` peer liveness and state transition rules. |
| `TestAdvancedFrameMatrix` | `G-10` 44-row Advanced protocol frame validation matrix. |
| `TestErrorCodeMapping` | `G-11` wire error code to sentinel mapping. |
| `TestGenerateDeterministic`, `TestCommittedVectorsCurrent` | Generator determinism and byte-identity with the committed tree. |

Note: `TestConstantsMatchSpec` explicitly pins two known differences between the legacy `spec/constants.json` (which records Protocol 1.0, `protocol_minor: 0`, encoded `256`, and omits error code `0x08`) and the active Protocol 1.1 implementation (`spec/ext/PROTOCOL_1_1_DRAFT.md`, `protocol_minor: 1`, and `ErrCodeShuttingDown = 0x08`).

## Using the Vectors When Porting HQQ

Port authors bringing up a new language implementation can validate their port incrementally following the conformance levels defined in [`spec/PORTING.md`](../spec/PORTING.md):

| Porting Level | Vector Groups to Verify |
| --- | --- |
| **L0: Layout** | `G-01` constants, `G-02` packets and payloads, `G-03` layouts, `G-04` geometry limits, and field offsets in `G-07`/`G-08` binary images. |
| **L1: Standard Opener (spin mode)** | `G-08` Opener and InMemory-join superblock cases, `G-07` ring `Read` and `Write` results, `G-09` peer states, `G-11` error codes on `Read`, and `G-05` name validation on open. |
| **L2: Standard Creator & Primary** | `G-06` golden lifecycle sequence (steps 1, 2, 5, and 6 with normalization), `G-08` Creator and InMemory-init cases, `G-07` ring attach validation, and `G-05` name creation and safe unlink rules. |
| **L3: OS Parking** | Verified dynamically via the two-process interop matrix (`PORTING.md` section 6.2, scenario `X-12`) and platform stress suites. |
| **L4: Advanced Protocol** | All 44 rows of `G-10`, Protocol 1.1 frames in `G-02`, `ShuttingDown` in `G-11`, and Protocol 1.1 metadata payloads. |

To run the binary image vectors in another language, parse `vectors/manifest.json` and iterate through each group:

- **Superblock cases (`superblocks`)**: Copy the `.bin` image into a page-aligned memory buffer (zero-extending to the full segment size unless `full` is `true`), invoke your port's Creator, Opener, or InMemory attach entry point, and verify that the returned status and mutation kind (`none`, `join`, or `init`) match the manifest expectation.
- **Advanced frame cases (`advanced_frames`)**: Feed the sequence of 64-byte frames to your receive state machine in order and verify that the resulting delivered messages, protocol errors, or silent drops match the manifest row.

## Adding an Implementation to the Interop Matrix

Once a port passes the static conformance vectors, wire it into the two-process cross-language test driver (`interop/`) in six steps:

1. **Implement the CLI contract**: Build a helper program following [`conformance/INTEROP_CLI.md`](INTEROP_CLI.md), supporting the `list`, `run`, and `version` subcommands, file barriers, deterministic payload generation, and canonical `error_kind` names.
2. **Verify `list` output**: Produce an executable named `hqq-interop` (`hqq-interop.exe` on Windows) and verify that `hqq-interop list --impl <label>` emits valid JSON matching section 7 of `INTEROP_CLI.md`. The test driver runs a scenario on a pair of implementations whenever both helpers list that scenario ID.
3. **Register via `HQQ_INTEROP_IMPLS`**: Set `HQQ_INTEROP_IMPLS` to a comma-separated list of `label=path` pairs (where labels match `[a-z0-9]+`), such as `HQQ_INTEROP_IMPLS="go=,c=build-c/tools/hqq-interop"`. Leaving `go=` empty builds the Go reference helper automatically from `./cmd/hqq-interop`.
4. **Enable soak sizes (optional)**: Set `HQQ_INTEROP_LONG=1` to run the extended message counts used in the scheduled CI soak job.
5. **Add to CI**: Add a build step for the new helper in `.github/workflows/interop.yml` and append `,<label>=<path>` to `HQQ_INTEROP_IMPLS` in the matrix execution step.
6. **Run the matrix locally**:

```bash
HQQ_INTEROP_IMPLS="go=,c=build-c/tools/hqq-interop" go test ./interop/... -count=1 -timeout 30m
```
