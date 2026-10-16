// Shared-memory links need 64-bit atomic operations that are atomic across
// processes. On 32-bit MIPS (mips, mipsle) and on ARM before ARMv7
// (GOARM=5 or 6) Go implements them with locks that are private to the
// process, so two processes sharing a segment would not exclude each other.
// Building for those targets fails with a message naming this file.

//go:build mips || mipsle || (arm && !arm.7)

package hqq

// hqq_requires_native_64bit_atomics is deliberately undefined: HQQ cannot be
// built for GOARCH=mips, GOARCH=mipsle or GOARCH=arm with GOARM below 7,
// because their 64-bit atomics are emulated with process-local locks and are
// not atomic across the processes that share a segment.
var _ = hqq_requires_native_64bit_atomics
