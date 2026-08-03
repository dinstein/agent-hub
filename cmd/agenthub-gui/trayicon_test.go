package main

import (
	"bytes"
	"image"
	"image/png"
	"math"
	"testing"
)

func decodeIcon(t *testing.T, state trayIcon, size int, lightGlyph bool) image.Image {
	t.Helper()
	raw, err := trayIconPNG(state, size, lightGlyph)
	if err != nil {
		t.Fatalf("trayIconPNG(%v): %v", state, err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decoding the %v icon: %v", state, err)
	}
	if got := img.Bounds().Dx(); got != size {
		t.Fatalf("%v icon is %d px wide, want %d", state, got, size)
	}
	return img
}

func TestTrayIconRendersEveryState(t *testing.T) {
	t.Parallel()
	const size = 32
	px := func(unit float64) int { return int(unit * float64(size)) }
	seen := map[string]trayIcon{}
	for _, state := range []trayIcon{trayIconOffline, trayIconAttention, trayIconOK} {
		img := decodeIcon(t, state, size, false)

		// The corners belong to the menu bar, not to us.
		if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
			t.Errorf("%v icon paints its top-left corner", state)
		}
		// The centre says what the state is: solid when serving, hollow when
		// there is nothing to serve. The mark's centre, not the image's — the
		// two are one optical shift apart.
		_, _, _, centre := img.At(size/2, px(iconCentreY)).RGBA()
		if state == trayIconOffline && centre != 0 {
			t.Errorf("offline icon has a filled centre; it must read as hollow")
		}
		if state != trayIconOffline && centre == 0 {
			t.Errorf("%v icon has a hollow centre; it must read as serving", state)
		}

		raw, err := trayIconPNG(state, size, false)
		if err != nil {
			t.Fatal(err)
		}
		if other, dup := seen[string(raw)]; dup {
			t.Fatalf("%v and %v render the same image", state, other)
		}
		seen[string(raw)] = state
	}
}

func TestTrayIconAttentionCarriesTheBadge(t *testing.T) {
	t.Parallel()
	const size = 64
	ok := decodeIcon(t, trayIconOK, size, false)
	attention := decodeIcon(t, trayIconAttention, size, false)

	px := func(unit float64) int { return int(unit * float64(size)) }
	if _, _, _, a := attention.At(px(iconBadgeX), px(iconBadgeY+iconOpticalShift)).RGBA(); a == 0 {
		t.Fatal("the attention badge is not painted")
	}
	// The badge is not merely drawn, it is cut free of the hub — otherwise
	// the two merge into one blob at menu-bar size. So there must be pixels
	// only the badge paints, and pixels the cut takes away.
	var badgeOnly, cutAway int
	for y := range size {
		for x := range size {
			_, _, _, aOK := ok.At(x, y).RGBA()
			_, _, _, aAttention := attention.At(x, y).RGBA()
			switch {
			case aAttention > 0 && aOK == 0:
				badgeOnly++
				if x < size/2 || y > size/2 {
					t.Fatalf("the badge painted outside its corner, at %d,%d", x, y)
				}
			case aOK > 0 && aAttention == 0:
				cutAway++
			}
		}
	}
	if badgeOnly == 0 {
		t.Error("the badge adds nothing the healthy icon does not already paint")
	}
	if cutAway == 0 {
		t.Error("the badge is not cut free of the hub; the two will merge")
	}
}

// TestTrayIconOfflineKeepsTheHubVisible guards the failure that killed the
// first draft of this mark: a hollow centre small enough that offline and
// serving render as the same picture at menu-bar size. Hollow means an
// outline, not an absence, and it has to survive 16 pixels.
func TestTrayIconOfflineKeepsTheHubVisible(t *testing.T) {
	t.Parallel()
	for _, size := range []int{16, 22, 44} {
		img := decodeIcon(t, trayIconOffline, size, false)
		px := func(unit float64) int { return int(unit * float64(size)) }

		centre := px(iconCentreY)
		edge := px(0.5 - iconCoreHalf + iconCoreStroke/2)
		if _, _, _, a := img.At(edge, centre).RGBA(); a == 0 {
			t.Errorf("at %d px the offline hub has no outline left", size)
		}
		hole := px(0.5 - (iconCoreHalf - iconCoreStroke) + 0.02)
		if _, _, _, a := img.At(hole, centre).RGBA(); a != 0 {
			t.Errorf("at %d px the offline hub's hole is filled in", size)
		}
	}
}

// TestTrayIconAlwaysDrawsItsClients holds the part of the mark that does NOT
// change: three clients converging on the hub, in every state. Only the hub
// answers what the state is, so a state that dropped a node would be saying
// something the tray does not mean — and it would also mean the attention
// badge's cut had eaten one.
func TestTrayIconAlwaysDrawsItsClients(t *testing.T) {
	t.Parallel()
	const size = 44
	for _, state := range []trayIcon{trayIconOffline, trayIconAttention, trayIconOK} {
		img := decodeIcon(t, state, size, false)
		for i := range iconNodeCount {
			a := -math.Pi/2 + float64(i)*2*math.Pi/iconNodeCount
			x := int((0.5 + math.Cos(a)*iconNodeOrbit) * size)
			y := int((iconCentreY + math.Sin(a)*iconNodeOrbit) * size)
			if _, _, _, alpha := img.At(x, y).RGBA(); alpha == 0 {
				t.Errorf("%v icon is missing the client node at %d,%d", state, x, y)
			}
		}
	}
}

func TestTrayIconGlyphColour(t *testing.T) {
	t.Parallel()
	const size = 32
	px := func(unit float64) int { return int(unit * float64(size)) }
	dark := decodeIcon(t, trayIconOK, size, false)
	light := decodeIcon(t, trayIconOK, size, true)

	r, g, b, a := dark.At(size/2, px(iconCentreY)).RGBA()
	if a == 0 || r != 0 || g != 0 || b != 0 {
		t.Errorf("light-background glyph is not black: %d %d %d %d", r, g, b, a)
	}
	r, g, b, a = light.At(size/2, px(iconCentreY)).RGBA()
	if a == 0 || r != a || g != a || b != a {
		t.Errorf("dark-background glyph is not white: %d %d %d %d", r, g, b, a)
	}
}

// TestTrayIconSitsInTheMiddleOfItsBox holds the correction that produced
// iconOpticalShift. One node above the hub and two below makes the mark's
// bounding box end short at the bottom, so drawing it about the box's own
// centre hangs it high in the menu bar with a gap underneath — the kind of
// misalignment that is obvious beside a neighbouring icon and invisible in
// isolation. Only the states whose outline is symmetric are checked: the
// attention badge is deliberately off to one shoulder.
func TestTrayIconSitsInTheMiddleOfItsBox(t *testing.T) {
	t.Parallel()
	const size = 128
	for _, state := range []trayIcon{trayIconOffline, trayIconOK} {
		img := decodeIcon(t, state, size, false)

		minX, minY, maxX, maxY := size, size, -1, -1
		for y := range size {
			for x := range size {
				if _, _, _, a := img.At(x, y).RGBA(); a == 0 {
					continue
				}
				minX, minY = min(minX, x), min(minY, y)
				maxX, maxY = max(maxX, x), max(maxY, y)
			}
		}
		if maxX < 0 {
			t.Fatalf("%v icon painted nothing at all", state)
		}
		// One pixel of slack for the rounding of an antialiased edge. The
		// bug this guards sat the mark twelve pixels high at this size:
		// spans 2..101 of 128 rather than 14..113.
		if off := (minX + maxX) - (size - 1); off < -1 || off > 1 {
			t.Errorf("%v icon is off-centre horizontally: spans %d..%d of %d", state, minX, maxX, size)
		}
		if off := (minY + maxY) - (size - 1); off < -1 || off > 1 {
			t.Errorf("%v icon is off-centre vertically: spans %d..%d of %d", state, minY, maxY, size)
		}
	}
}

func TestTrayIconRejectsAnEmptyCanvas(t *testing.T) {
	t.Parallel()
	if _, err := trayIconPNG(trayIconOK, 0, false); err == nil {
		t.Fatal("a zero-sized icon was accepted; it would install a blank image")
	}
}
