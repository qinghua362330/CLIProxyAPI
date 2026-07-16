//go:build cgo

package executor

import (
	"bytes"
	"io"

	ddzstd "github.com/DataDog/zstd"
)

var codexOfficialZstdFramePrefix = []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x58}

type codexZstdWriteCloser interface {
	io.Writer
	io.Closer
}

type codexZstdWriterFactory func(io.Writer) codexZstdWriteCloser

// codexZstdCompress mirrors Codex rust-v0.144.5's
// zstd::stream::encode_all(Cursor, 3) with the same libzstd 1.5.7 streaming
// implementation. The request body is published only after the complete frame
// has been written and closed successfully.
func codexZstdCompress(src []byte) ([]byte, bool) {
	return codexZstdCompressWithWriter(src, func(dst io.Writer) codexZstdWriteCloser {
		return ddzstd.NewWriterLevel(dst, 3)
	})
}

func codexZstdCompressWithWriter(src []byte, newWriter codexZstdWriterFactory) ([]byte, bool) {
	if newWriter == nil {
		return nil, false
	}

	var dst bytes.Buffer
	writer := newWriter(&dst)
	if writer == nil {
		return nil, false
	}

	written, writeErr := writer.Write(src)
	// DataDog's public API cannot forcibly reclaim a stream after an internal
	// libzstd firstError. The private bytes.Buffer removes ordinary destination
	// write failures; Close is still called exactly once to preserve wire safety.
	closeErr := writer.Close()
	if writeErr != nil || written != len(src) || closeErr != nil {
		return nil, false
	}

	compressed := dst.Bytes()
	if len(src) > 0 && !bytes.HasPrefix(compressed, codexOfficialZstdFramePrefix) {
		return nil, false
	}
	return compressed, true
}
