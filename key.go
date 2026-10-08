package uploads

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxNameBytes is the longest file name most filesystems accept.
const maxNameBytes = 255

// newKey returns a fresh storage key: 32 hex characters of crypto/rand (128
// bits) followed by the extension for the sniffed content type. The key never
// carries anything the client sent.
func newKey(sniffed string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]) + extensionFor(sniffed), nil
}

// extensionFor maps a sniffed content type, parameters allowed (as
// http.DetectContentType returns them), to the extension a key gets.
// Anything not on the short list is ".bin", so a stored file never gets an
// extension a browser or server would treat as active content.
func extensionFor(contentType string) string {
	media, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(media)) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	default:
		return ".bin"
	}
}

// cleanName turns a client-supplied file name into something safe to show or
// keep as metadata. It is never used as a path. It keeps only the part after
// the last "/" or "\", drops invalid UTF-8 and control characters, maps "."
// and ".." to "", and truncates to 255 bytes on a rune boundary.
func cleanName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ToValidUTF8(name, "")
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	if name == "." || name == ".." {
		return ""
	}
	if len(name) > maxNameBytes {
		cut := maxNameBytes
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = name[:cut]
	}
	return name
}

// validKey reports whether key is safe to join to a store's directory. It
// refuses the empty key, keys longer than 255 bytes, invalid UTF-8, "/", "\",
// "..", control characters, and a leading "." (which also covers "." itself
// and keeps Put's ".upload-*" temp files unreachable by key).
func validKey(key string) bool {
	if key == "" || len(key) > maxNameBytes || !utf8.ValidString(key) {
		return false
	}
	if key[0] == '.' || strings.Contains(key, "..") || strings.ContainsAny(key, `/\`) {
		return false
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
