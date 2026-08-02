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
// The shapes are deliberately crude, because a menu-bar icon is 16-22 points
// across and anything with detail turns to mud: a ring for "the hub exists",
// a filled centre for "it is serving", a badge for "something wants you".

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Geometry, in units of the icon's width. The ring is the constant across
// every state so the icon reads as one family; only what is inside it and
// what sits on its shoulder change.
const (
	iconRingOuter = 0.44
	iconRingInner = 0.31
	// The offline ring is thinner — a hairline outline is the flattest thing
	// that still says "there is a hub here", and it must not be mistakeable
	// for the solid connected mark at a glance.
	iconRingInnerOffline = 0.36
	iconCoreRadius       = 0.16
	iconBadgeRadius      = 0.18
	// The badge is separated from the ring by a cut rather than by distance:
	// at 22 pixels there is no room to move it away, so the ring is carved
	// back instead and the gap reads as depth. The badge sits far enough out
	// that the cut reaches the ring and stops short of the core — a bite
	// taken out of the middle would make the icon read as a fourth state.
	iconBadgeGap = 0.05
	iconBadgeX   = 0.80
	iconBadgeY   = 0.20
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
// square. Everything the three states differ by is in this function.
func iconCovers(state trayIcon, x, y float64) bool {
	fromCentre := math.Hypot(x-0.5, y-0.5)
	inner := iconRingInner
	if state == trayIconOffline {
		inner = iconRingInnerOffline
	}
	inRing := fromCentre <= iconRingOuter && fromCentre >= inner
	inCore := state != trayIconOffline && fromCentre <= iconCoreRadius

	if state != trayIconAttention {
		return inRing || inCore
	}

	fromBadge := math.Hypot(x-iconBadgeX, y-iconBadgeY)
	if fromBadge <= iconBadgeRadius {
		return true
	}
	// The cut: nothing else is drawn within the badge's halo, which is what
	// keeps the badge visible where it overlaps the ring.
	if fromBadge <= iconBadgeRadius+iconBadgeGap {
		return false
	}
	return inRing || inCore
}
