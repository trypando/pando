// Package qrcode draws QR codes: an SVG for the console and API, and text for a
// terminal. Used for the authenticator app setup of R-405.
package qrcode

import (
	"fmt"
	"strings"

	"rsc.io/qr"
)

// quietZone is the blank border, in modules, that a QR reader needs to find
// the code's edges (ISO/IEC 18004 asks for four).
const quietZone = 4

// SVG draws text as a QR code, an SVG of black modules on white. Drawn here
// rather than in the console so every client of the API gets the same picture,
// and black on white whatever the page's theme, because that is what a phone's
// camera reads most reliably.
func SVG(text string) (string, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	side := c.Size + 2*quietZone
	var path strings.Builder
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.Black(x, y) {
				fmt.Fprintf(&path, "M%d %dh1v1h-1z", x+quietZone, y+quietZone)
			}
		}
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`+
		`<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="%s"/></svg>`,
		side, side, side, side, path.String()), nil
}

// Text draws text as a QR code in a terminal, two rows of modules to a line
// with half-block characters, for `pando two-factor setup`. Dark modules are
// spaces and light ones are filled, so on a dark terminal the code reads as
// black on white; the quiet zone is drawn too, since a terminal's own
// background is not one.
func Text(text string) (string, error) {
	c, err := qr.Encode(text, qr.L)
	if err != nil {
		return "", err
	}
	light := func(x, y int) bool {
		x, y = x-quietZone, y-quietZone
		return x < 0 || y < 0 || x >= c.Size || y >= c.Size || !c.Black(x, y)
	}
	side := c.Size + 2*quietZone
	var b strings.Builder
	for y := 0; y < side; y += 2 {
		for x := 0; x < side; x++ {
			top, bottom := light(x, y), y+1 >= side || light(x, y+1)
			switch {
			case top && bottom:
				b.WriteRune('█')
			case top:
				b.WriteRune('▀')
			case bottom:
				b.WriteRune('▄')
			default:
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}
