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
// the last "/" or "\", drops invalid UTF-8, control characters (Cc), format
// characters (Cf, such as U+202E RIGHT-TO-LEFT OVERRIDE) and line and
// paragraph separators (Zl, Zp), maps "."
// and ".." to "", and truncates to 255 bytes on a rune boundary.
func cleanName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ToValidUTF8(name, "")
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
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

// validKey reports whether key is safe to join to a store's directory on any
// operating system. It is an explicit allowlist, the same on every OS, rather
// than filepath.IsLocal, whose answer differs between Unix and Windows. A key:
//   - is 1 to 255 bytes of [a-z0-9_.-] only (no uppercase, so two keys never
//     name one file on a case-insensitive filesystem);
//   - neither starts nor ends with "." (a leading dot keeps Put's ".upload-*"
//     temp files unreachable; Windows drops a trailing one) and has no "..";
//   - is not a Windows reserved device name (con, prn, aux, nul, com1-9,
//     lpt1-9), with or without an extension.
//
// Every key newKey returns passes.
func validKey(key string) bool {
	if key == "" || len(key) > maxNameBytes {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	if key[0] == '.' || key[len(key)-1] == '.' || strings.Contains(key, "..") {
		return false
	}
	return !windowsReserved(key)
}

// windowsReserved reports whether key's part before the first "." is a
// Windows device name. Windows opens the device for "nul" and "nul.png" alike.
func windowsReserved(key string) bool {
	stem, _, _ := strings.Cut(strings.ToLower(key), ".")
	switch stem {
	case "con", "prn", "aux", "nul":
		return true
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "com") || strings.HasPrefix(stem, "lpt")) {
		return '1' <= stem[3] && stem[3] <= '9'
	}
	return false
}
