//! Deterministic payloads (INTEROP_CLI.md, section 5): SplitMix64 bodies, CRC32-IEEE headers.

/// CRC32 (IEEE 802.3, reflected, polynomial 0xEDB88320).
fn crc32(data: &[u8]) -> u32 {
    let mut crc = 0xFFFF_FFFFu32;
    for &byte in data {
        crc ^= u32::from(byte);
        for _ in 0..8 {
            let mask = (crc & 1).wrapping_neg();
            crc = (crc >> 1) ^ (0xEDB8_8320 & mask);
        }
    }
    !crc
}

/// The SplitMix64 stream of `(seed, seq)`.
struct SplitMix {
    state: u64,
}

impl SplitMix {
    fn new(seed: u64, seq: u64) -> SplitMix {
        SplitMix {
            state: seed ^ seq.wrapping_mul(0xD1B5_4A32_D192_ED03),
        }
    }

    fn next(&mut self) -> u64 {
        self.state = self.state.wrapping_add(0x9E37_79B9_7F4A_7C15);
        let mut z = self.state;
        z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
        z ^ (z >> 31)
    }
}

/// The first `n` bytes of the concatenated little-endian words of the stream.
fn body(seed: u64, seq: u64, n: usize) -> Vec<u8> {
    let mut rng = SplitMix::new(seed, seq);
    let mut out = Vec::with_capacity(n + 8);
    while out.len() < n {
        out.extend_from_slice(&rng.next().to_le_bytes());
    }
    out.truncate(n);
    out
}

/// `Payload(seed, seq, size)`: empty, a bare body below 16 bytes, otherwise
/// `seq` (u64 LE) | crc32(body) (u32 LE) | body.
pub fn payload(seed: u64, seq: u64, size: usize) -> Vec<u8> {
    match size {
        0 => Vec::new(),
        1..=15 => body(seed, seq, size),
        _ => {
            let b = body(seed, seq, size - 12);
            let mut out = Vec::with_capacity(size);
            out.extend_from_slice(&seq.to_le_bytes());
            out.extend_from_slice(&crc32(&b).to_le_bytes());
            out.extend_from_slice(&b);
            out
        }
    }
}

/// True when `data` is exactly message `seq` of stream `seed` with `size` bytes.
pub fn intact(seed: u64, seq: u64, size: usize, data: &[u8]) -> bool {
    data.len() == size && data == payload(seed, seq, size).as_slice()
}
