package transfer

import (
	"encoding/base64"
	"hash/crc32"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// copyBufferBytes is the one buffer size a transfer moves bytes with: the file
// streams through it, and no transfer ever holds more of a file than this —
// a tus chunk aside, which is bounded by its own setting.
const copyBufferBytes = 64 << 10

// sniffBytes is how much of a file content sniffing reads: the WHATWG
// algorithm's own window.
const sniffBytes = 512

// castagnoli is CRC32C's table — the checksum GCS keeps for every object, so
// the platform verifies a whole upload without a second read (decision 5).
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// CRC32C is a reader's CRC32C, streamed through a fixed buffer, as GCS writes
// it: the four bytes big-endian, in base64. It also returns how many bytes it
// read.
func CRC32C(r io.Reader) (string, int64, error) {
	hash := crc32.New(castagnoli)
	n, err := io.CopyBuffer(hash, r, make([]byte, copyBufferBytes))
	if err != nil {
		return "", n, err
	}
	return base64.StdEncoding.EncodeToString(hash.Sum(nil)), n, nil
}

// mediaType is the platform's shape for a content type: `type/subtype`, no
// parameters.
var mediaType = regexp.MustCompile(`^[\w.+-]+/[\w.+-]+$`)

// octetStream is the type of bytes nothing more is known about.
const octetStream = "application/octet-stream"

// SniffContentType names a file's type from its first bytes — its magic
// numbers, by the WHATWG sniffing algorithm — never from its name: a name is
// the harness's to choose, and a person opening the file gets what the bytes
// are. Parameters are dropped (`text/plain; charset=utf-8` is `text/plain`).
func SniffContentType(r io.Reader) (string, error) {
	head := make([]byte, sniffBytes)
	n, err := io.ReadFull(r, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	return cleanContentType(http.DetectContentType(head[:n])), nil
}

// cleanContentType reduces a content type to `type/subtype`, or
// application/octet-stream when it is not one.
func cleanContentType(value string) string {
	if i := strings.IndexByte(value, ';'); i >= 0 {
		value = value[:i]
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if !mediaType.MatchString(value) || len(value) > 127 {
		return octetStream
	}
	return value
}
