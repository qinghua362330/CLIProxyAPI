//go:build !cgo

package executor

// Non-CGO builds remain supported and deliberately send plaintext rather than
// substituting a zstd implementation with a different wire fingerprint.
func codexZstdCompress([]byte) ([]byte, bool) {
	return nil, false
}
