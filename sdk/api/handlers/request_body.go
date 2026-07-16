package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	log "github.com/sirupsen/logrus"
)

// slowRequestBodyThreshold bounds the noise from the read-timing log below.
// Reading an already-buffered body is sub-millisecond, so anything past this is
// the client still sending -- not this process being slow.
const slowRequestBodyThreshold = 3 * time.Second

// ReadRequestBody reads the incoming request body and decodes supported
// Content-Encoding values before handlers inspect JSON fields.
func ReadRequestBody(c *gin.Context) ([]byte, error) {
	// GetRawData blocks until the client finishes sending. That wait lands inside
	// the request's total time but before any upstream call, so a slow sender is
	// indistinguishable from a slow upstream unless it is measured here.
	readStart := time.Now()
	raw, err := c.GetRawData()
	readElapsed := time.Since(readStart)
	if err != nil {
		return nil, err
	}
	if readElapsed >= slowRequestBodyThreshold {
		rate := ""
		if secs := readElapsed.Seconds(); secs > 0 && len(raw) > 0 {
			rate = fmt.Sprintf(", %.0f KB/s", float64(len(raw))/1024/secs)
		}
		path := ""
		if c != nil && c.Request != nil {
			path = c.Request.URL.Path
		}
		log.Warnf("slow request body read: %s for %d bytes%s (path=%s) -- the client was still sending; this is upstream of any provider call",
			readElapsed.Round(time.Millisecond), len(raw), rate, path)
	}

	encoding := ""
	if c != nil && c.Request != nil {
		encoding = strings.TrimSpace(c.Request.Header.Get("Content-Encoding"))
	}
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw, nil
	}

	decoded, err := decodeRequestBody(raw, encoding)
	if err != nil {
		if json.Valid(raw) {
			return raw, nil
		}
		return nil, err
	}
	return decoded, nil
}

func decodeRequestBody(raw []byte, encoding string) ([]byte, error) {
	parts := strings.Split(encoding, ",")
	body := raw
	for i := len(parts) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(parts[i]))
		switch enc {
		case "", "identity":
			continue
		case "zstd":
			decoded, err := decodeZstdRequestBody(body)
			if err != nil {
				return nil, err
			}
			body = decoded
		default:
			return nil, fmt.Errorf("unsupported request content encoding: %s", enc)
		}
	}
	return body, nil
}

func decodeZstdRequestBody(raw []byte) ([]byte, error) {
	decoder, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd request decoder: %w", err)
	}
	defer decoder.Close()

	decoded, err := io.ReadAll(decoder)
	if err != nil {
		return nil, fmt.Errorf("failed to decode zstd request body: %w", err)
	}
	return decoded, nil
}
