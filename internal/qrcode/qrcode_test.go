package qrcode

import (
	"strings"
	"testing"
)

func TestSVGIsBlackOnWhiteWithAQuietZone(t *testing.T) {
	svg, err := SVG("otpauth://totp/Pando:ben?secret=ABC&issuer=Pando")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<svg xmlns="http://www.w3.org/2000/svg"`, `fill="#fff"`, `fill="#000"`, "M4 4h1v1h-1z"} {
		if !strings.Contains(svg, want) {
			t.Errorf("svg has no %q", want)
		}
	}
}

func TestTextIsSquare(t *testing.T) {
	out, err := Text("otpauth://totp/Pando:ben?secret=ABC")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	width := len([]rune(lines[0]))
	if want := (width + 1) / 2; len(lines) != want {
		t.Errorf("%d lines for %d columns, want %d", len(lines), width, want)
	}
	if !strings.HasPrefix(lines[0], "████") {
		t.Errorf("first line %q does not start with the quiet zone", lines[0])
	}
}
