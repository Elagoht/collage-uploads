package uploads

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNewKey(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9a-f]{32}\.png$`)
	seen := make(map[string]bool, 1000)
	for range 1000 {
		key, err := newKey("image/png")
		if err != nil {
			t.Fatalf("newKey: %v", err)
		}
		if !shape.MatchString(key) {
			t.Fatalf("key %q has the wrong shape", key)
		}
		if !validKey(key) {
			t.Fatalf("key %q is refused by validKey", key)
		}
		if seen[key] {
			t.Fatalf("key %q repeated", key)
		}
		seen[key] = true
	}
}

func TestExtensionFor(t *testing.T) {
	cases := map[string]string{
		"image/png":                 ".png",
		"image/jpeg":                ".jpg",
		"image/gif":                 ".gif",
		"image/webp":                ".webp",
		"application/pdf":           ".pdf",
		"text/plain; charset=utf-8": ".txt",
		"TEXT/PLAIN":                ".txt",
		"text/html; charset=utf-8":  ".bin",
		"image/svg+xml":             ".bin",
		"application/octet-stream":  ".bin",
		"":                          ".bin",
	}
	for in, want := range cases {
		if got := extensionFor(in); got != want {
			t.Errorf("extensionFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"../../etc/passwd", "passwd"},
		{`a\b.png`, "b.png"},
		{`C:\Users\me\photo.jpg`, "photo.jpg"},
		{"re\x00po\nrt\x7f.pdf", "report.pdf"},
		{"dir/", ""},
		{"..", ""},
		{".", ""},
		{"", ""},
		{"plain.txt", "plain.txt"},
		{"bad\xffutf8.txt", "badutf8.txt"},
		{"evil\u202egnp.exe", "evilgnp.exe"},
		{"line\u2028sep\u2029para.txt", "lineseppara.txt"},
		{"zero\u200bwidth\ufeff.txt", "zerowidth.txt"},
	}
	for _, c := range cases {
		if got := cleanName(c.in); got != c.want {
			t.Errorf("cleanName(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	long := cleanName(strings.Repeat("ç", 300))
	if len(long) > 255 {
		t.Errorf("long name is %d bytes, want ≤255", len(long))
	}
	if !utf8.ValidString(long) {
		t.Errorf("long name is not valid UTF-8")
	}
	if long != strings.Repeat("ç", 127) {
		t.Errorf("long name truncated to %d bytes, want 254", len(long))
	}
}

func TestValidKey(t *testing.T) {
	good := []string{
		"0123456789abcdef0123456789abcdef.png", "report.pdf", "a", "a_b-c.tar.gz",
		"console.txt", "com0.txt", "com10", "lpt.txt", "nul_x",
	}
	for _, k := range good {
		if !validKey(k) {
			t.Errorf("validKey(%q) = false, want true", k)
		}
	}
	bad := []string{
		"", ".", "..", "../x", "a/b", `a\b`, "x\n", "x\x00", "x\x7f",
		".upload-123", ".hidden", "a..b", "bad\xff", strings.Repeat("a", 256),
		"CON", "con", "nul.png", "Nul.tar.gz", "prn", "aux.txt", "com1", "com9.bin", "lpt1", "lpt9.png",
		"x:y", "x.", "X.PNG", "Report.pdf", "çiçek.png", "x ", "a b", "a+b", "a%2f",
	}
	for _, k := range bad {
		if validKey(k) {
			t.Errorf("validKey(%q) = true, want false", k)
		}
	}
}
