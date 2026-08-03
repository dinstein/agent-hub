package main

// The tray icon, drawn rather than shipped.
//
// WHY NOT PNG FILES. Nine images would be needed — three states, and for each
// a light-background, a dark-background and a macOS template variant — and
// binary assets are the one kind of file nobody re-reads when the meaning of a
// state changes. Drawing them costs about a hundred lines of arithmetic, no
// new dependency (image/png is standard library), and makes the glyph a pure
// function that a test can hold to account.
//
// WHY NOT A RING. The first version of this file drew a bullseye: a ring for
// "the hub exists", a filled centre for "it is serving". It read well and it
// was the wrong mark twice over. It said nothing this product says — every
// second menu-bar utility owns a ring — and the other local MCP hubs put very
// nearly that same picture in the status area, one of them a thick ring with
// holes punched in it around a solid centre. A hub that looks like the hub
// next to it is not identification.
//
// WHAT IT DRAWS INSTEAD. The application icon's mark, reduced until it
// survives 22 points: the rounded-square hub, and the clients converging on
// it. Three of them, not the icon's six — six nodes at this size are a grey
// smudge, and the count was never the claim — and detached from the hub
// rather than joined to it by spokes, because at 22 points a spoke fuses the
// node, the arm and the core into one blob and the hub stops being findable.
// What each state changes is then the smallest possible thing: the hub is
// hollow when nothing is being served, solid when it is, and grows a badge on
// its shoulder when a server wants attention.

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Geometry, in units of the icon's width. The three nodes are the constant
// across every state so the icon reads as one family; only the hub they
// converge on changes, and what sits on its shoulder.
const (
	// The hub. Large enough that hollowing it leaves a hole that is still a
	// hole at 22 points — the failure of an earlier draft, whose two-pixel
	// centre made "offline" and "serving" the same picture.
	iconCoreHalf   = 0.20
	iconCoreRound  = 0.062
	iconCoreStroke = 0.058
	// The clients: one directly above the hub and two below it, the
	// arrangement the application icon uses, minus the three that do not
	// survive the size.
	iconNodeRadius = 0.11
	iconNodeOrbit  = 0.375
	iconNodeCount  = 3
	// The badge sits on the shoulder the nodes leave empty, and is separated
	// from what is under it by a cut rather than by distance: the clearance
	// to the hub's corner is a fraction of a pixel at 22 points, so leaving
	// the separation to the rounding would weld the two together at one size
	// and not at another. The cut is wider than that clearance on purpose.
	iconBadgeRadius = 0.155
	iconBadgeGap    = 0.055
	iconBadgeX      = 0.82
	iconBadgeY      = 0.185
)

// iconSupersample is the number of samples per pixel per axis. Four is enough
// that curved edges stop stair-stepping at 16 pixels and cheap enough that the
// whole icon renders in well under a millisecond.
const iconSupersample = 4

// trayIconPNG renders one state as a PNG.
//
// lightGlyph selects the foreground colour: white for a dark background
// (Windows' dark taskbar), black otherwise. macOS ignores it — a template icon
// carries only alpha and the system colours it — so darwin always asks for the
// black one and hands it to SetTemplateIcon.
func trayIconPNG(state trayIcon, size int, lightGlyph bool) ([]byte, error) {
	if size <= 0 {
		return nil, fmt.Errorf("tray icon size %d: must be positive", size)
	}
	fg := color.NRGBA{R: 0, G: 0, B: 0, A: 255}
	if lightGlyph {
		fg = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	}

	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	step := 1.0 / float64(size) / float64(iconSupersample)
	for py := range size {
		for px := range size {
			var hits int
			for sy := range iconSupersample {
				for sx := range iconSupersample {
					x := (float64(px)+0.5/float64(iconSupersample))/float64(size) + float64(sx)*step
					y := (float64(py)+0.5/float64(iconSupersample))/float64(size) + float64(sy)*step
					if iconCovers(state, x, y) {
						hits++
					}
				}
			}
			if hits == 0 {
				continue
			}
			c := fg
			c.A = uint8(hits * 255 / (iconSupersample * iconSupersample)) //nolint:gosec // bounded by the sample count
			img.SetNRGBA(px, py, c)
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encoding tray icon: %w", err)
	}
	return buf.Bytes(), nil
}

// iconCovers answers whether the glyph is opaque at one point of the unit
// square. Everything the three states differ by is in this function and the
// one below it.
func iconCovers(state trayIcon, x, y float64) bool {
	if state != trayIconAttention {
		return iconMarkCovers(state, x, y)
	}
	switch d := math.Hypot(x-iconBadgeX, y-iconBadgeY); {
	case d <= iconBadgeRadius:
		return true
	case d <= iconBadgeRadius+iconBadgeGap:
		// The cut: nothing else is drawn within the badge's halo, which is
		// what keeps the badge from merging into the hub beneath it.
		return false
	}
	return iconMarkCovers(state, x, y)
}

// iconMarkCovers is the mark without the badge: the hub, and the clients on
// their way to it.
func iconMarkCovers(state trayIcon, x, y float64) bool {
	if inRoundSquare(x, y, iconCoreHalf, iconCoreRound) {
		if state == trayIconOffline {
			// Hollow: there is a hub here, and it is serving nothing.
			return !inRoundSquare(x, y, iconCoreHalf-iconCoreStroke, iconCoreRound*0.6)
		}
		return true
	}
	for i := range iconNodeCount {
		// Straight up first, then a third of a turn at a time.
		a := -math.Pi/2 + float64(i)*2*math.Pi/iconNodeCount
		nx := 0.5 + math.Cos(a)*iconNodeOrbit
		ny := 0.5 + math.Sin(a)*iconNodeOrbit
		if math.Hypot(x-nx, y-ny) <= iconNodeRadius {
			return true
		}
	}
	return false
}

// inRoundSquare reports whether a point of the unit square falls inside a
// rounded square centred on the icon, with the given half-width and corner
// radius.
func inRoundSquare(x, y, half, round float64) bool {
	dx := math.Abs(x-0.5) - (half - round)
	dy := math.Abs(y-0.5) - (half - round)
	if dx <= 0 && dy <= 0 {
		return true
	}
	return math.Hypot(math.Max(dx, 0), math.Max(dy, 0)) <= round
}
