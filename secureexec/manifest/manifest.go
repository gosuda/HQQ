// Package manifest defines the signed release manifest that lists the SHA-256
// digests of release artifacts. Verify a manifest with the release public key
// before using its digests with secureexec.Verify.
//
// Format (manifest.json, version 1):
//
//	{
//	  "version": 1,
//	  "name": "hqq-sqlite-server",
//	  "release": "v0.6.0",
//	  "files": [
//	    {"os": "linux", "arch": "amd64", "name": "hqq-sqlite-server_linux_amd64",
//	     "sha256": "<64 hex digits>", "size": 1234}
//	  ],
//	  "licenses": ["MIT"]
//	}
//
// Signature (manifest.json.sig): the hex encoding of the 64-byte ed25519
// signature over the exact bytes of manifest.json, followed by a newline.
// Public keys are the hex encoding of the 32-byte ed25519 public key. Private
// keys are the hex encoding of the 32-byte ed25519 seed. Keep the private key
// in a CI secret (for example an environment variable read by
// hqq-manifest sign -key-env) and never in the repository.
package manifest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gosuda.org/hqq/secureexec"
)

// Version is the only manifest format version this package reads and writes.
const Version = 1

// ErrBadSignature reports a signature that does not verify.
var ErrBadSignature = errors.New("manifest: bad signature")

// Manifest lists the artifacts of one release.
type Manifest struct {
	Version  int      `json:"version"`
	Name     string   `json:"name"`
	Release  string   `json:"release"`
	Files    []File   `json:"files"`
	Licenses []string `json:"licenses"`
}

// File describes one artifact. Name is the file name inside the artifact directory.
type File struct {
	OS     string          `json:"os"`
	Arch   string          `json:"arch"`
	Name   string          `json:"name"`
	SHA256 secureexec.Hash `json:"sha256"`
	Size   int64           `json:"size"`
}

// Find returns the file for goos and goarch. The result is nil when there is none.
func (m *Manifest) Find(goos, goarch string) (*File, bool) {
	for i := range m.Files {
		if m.Files[i].OS == goos && m.Files[i].Arch == goarch {
			return &m.Files[i], true
		}
	}
	return nil, false
}

// Parse decodes and validates an unsigned manifest. Unknown fields, trailing
// data and unknown versions are rejected. Use Verify for untrusted input:
// it checks the signature before parsing.
func Parse(data []byte) (*Manifest, error) {
	var head struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if head.Version == nil {
		return nil, errors.New("manifest: missing version")
	}
	if *head.Version != Version {
		return nil, fmt.Errorf("manifest: unsupported version %d (want %d)", *head.Version, Version)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("manifest: trailing data after JSON value")
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Marshal validates m and encodes it as indented JSON with a trailing newline.
func Marshal(m *Manifest) ([]byte, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func (m *Manifest) validate() error {
	if m.Version != Version {
		return fmt.Errorf("manifest: unsupported version %d (want %d)", m.Version, Version)
	}
	if err := checkToken("name", m.Name); err != nil {
		return err
	}
	if m.Release == "" {
		return errors.New("manifest: empty release")
	}
	if len(m.Files) == 0 {
		return errors.New("manifest: no files")
	}
	seen := make(map[[2]string]bool, len(m.Files))
	names := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		if err := checkToken("os", f.OS); err != nil {
			return err
		}
		if err := checkToken("arch", f.Arch); err != nil {
			return err
		}
		if f.Name == "" || f.Name == "." || f.Name == ".." || strings.ContainsAny(f.Name, `/\`) {
			return fmt.Errorf("manifest: file name %q must be a plain file name", f.Name)
		}
		if f.Size < 0 {
			return fmt.Errorf("manifest: file %s: negative size", f.Name)
		}
		key := [2]string{f.OS, f.Arch}
		if seen[key] {
			return fmt.Errorf("manifest: duplicate entry for %s/%s", f.OS, f.Arch)
		}
		if names[f.Name] {
			return fmt.Errorf("manifest: duplicate file name %s", f.Name)
		}
		seen[key] = true
		names[f.Name] = true
	}
	for _, l := range m.Licenses {
		if l == "" {
			return errors.New("manifest: empty license identifier")
		}
	}
	return nil
}

func checkToken(field, v string) error {
	if v == "" || strings.ContainsAny(v, " \t\r\n/\\") {
		return fmt.Errorf("manifest: %s %q is empty or contains a separator", field, v)
	}
	return nil
}

// Sign returns the detached signature text for data: hex of the ed25519
// signature followed by a newline.
func Sign(data []byte, key ed25519.PrivateKey) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("manifest: bad private key length")
	}
	return []byte(hex.EncodeToString(ed25519.Sign(key, data)) + "\n"), nil
}

// Verify checks the detached signature sig (the text written by Sign) over
// data with pub, then parses data. It returns ErrBadSignature on a mismatch.
func Verify(data, sig []byte, pub ed25519.PublicKey) (*Manifest, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("manifest: bad public key length")
	}
	raw, err := hex.DecodeString(string(bytes.TrimSpace(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return nil, fmt.Errorf("manifest: malformed signature: %w", ErrBadSignature)
	}
	if !ed25519.Verify(pub, data, raw) {
		return nil, ErrBadSignature
	}
	return Parse(data)
}

// ParsePrivateKey parses the hex seed text written by FormatPrivateKey.
func ParsePrivateKey(text []byte) (ed25519.PrivateKey, error) {
	seed, err := decodeHexKey(text, ed25519.SeedSize)
	if err != nil {
		return nil, fmt.Errorf("manifest: private key: %w", err)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// ParsePublicKey parses the hex text written by FormatPublicKey.
func ParsePublicKey(text []byte) (ed25519.PublicKey, error) {
	raw, err := decodeHexKey(text, ed25519.PublicKeySize)
	if err != nil {
		return nil, fmt.Errorf("manifest: public key: %w", err)
	}
	return ed25519.PublicKey(raw), nil
}

func decodeHexKey(text []byte, size int) ([]byte, error) {
	raw, err := hex.DecodeString(string(bytes.TrimSpace(text)))
	if err != nil {
		return nil, err
	}
	if len(raw) != size {
		return nil, fmt.Errorf("want %d bytes, got %d", size, len(raw))
	}
	return raw, nil
}

// FormatPrivateKey returns the hex seed text of priv, with a newline.
func FormatPrivateKey(priv ed25519.PrivateKey) []byte {
	return []byte(hex.EncodeToString(priv.Seed()) + "\n")
}

// FormatPublicKey returns the hex text of pub, with a newline.
func FormatPublicKey(pub ed25519.PublicKey) []byte {
	return []byte(hex.EncodeToString(pub) + "\n")
}

// GenerateKey creates a new ed25519 key pair from crypto/rand.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// Scan builds a manifest from the artifacts in dir. An artifact is a regular
// file named <product>_<os>_<arch> (optionally with a .exe suffix). Other
// files are ignored, but a file that starts with "<product>_" and does not
// match the pattern is an error. Symbolic links are refused. Files are sorted
// by name. Scan does not sign.
func Scan(dir, product, release string, licenses []string) (*Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []File
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, product+"_") {
			continue
		}
		full := filepath.Join(dir, name)
		fi, err := os.Lstat(full)
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("manifest: artifact %s: not a regular file", full)
		}
		goos, goarch, err := parseArtifactName(product, name)
		if err != nil {
			return nil, err
		}
		sum, size, err := secureexec.HashFile(full)
		if err != nil {
			return nil, err
		}
		files = append(files, File{OS: goos, Arch: goarch, Name: name, SHA256: sum, Size: size})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("manifest: no %s_<os>_<arch> artifacts in %s", product, dir)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	m := &Manifest{Version: Version, Name: product, Release: release, Files: files, Licenses: licenses}
	if _, err := Marshal(m); err != nil {
		return nil, err
	}
	return m, nil
}

// parseArtifactName extracts os and arch from <product>_<os>_<arch>[.exe].
func parseArtifactName(product, name string) (string, string, error) {
	rest := strings.TrimSuffix(strings.TrimPrefix(name, product+"_"), ".exe")
	goos, goarch, ok := strings.Cut(rest, "_")
	if !ok || goos == "" || goarch == "" || strings.Contains(goarch, "_") {
		return "", "", fmt.Errorf("manifest: artifact %q: want %s_<os>_<arch>", name, product)
	}
	return goos, goarch, nil
}

// VerifyFiles checks every artifact listed in m against the files in dir:
// size and SHA-256 must match.
func VerifyFiles(dir string, m *Manifest) error {
	for _, f := range m.Files {
		sum, size, err := secureexec.HashFile(filepath.Join(dir, f.Name))
		if err != nil {
			return fmt.Errorf("manifest: %s: %w", f.Name, err)
		}
		if size != f.Size {
			return fmt.Errorf("manifest: %s: size %d, manifest says %d", f.Name, size, f.Size)
		}
		if sum != f.SHA256 {
			return fmt.Errorf("manifest: %s: %w: got %s, want %s", f.Name, secureexec.ErrHashMismatch, sum, f.SHA256)
		}
	}
	return nil
}
