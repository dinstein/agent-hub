package main

import (
	"bytes"
	"image"
	"image/png"
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
	seen := map[string]trayIcon{}
	for _, state := range []trayIcon{trayIconOffline, trayIconAttention, trayIconOK} {
		img := decodeIcon(t, state, size, false)

		// The corners belong to the menu bar, not to us.
		if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
			t.Errorf("%v icon paints its top-left corner", state)
		}
		// The centre says what the state is: solid when serving, hollow when
		// there is nothing to serve.
		_, _, _, centre := img.At(size/2, size/2).RGBA()
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
	if _, _, _, a := attention.At(px(iconBadgeX), px(iconBadgeY)).RGBA(); a == 0 {
		t.Fatal("the attention badge is not painted")
	}
	// The badge is not merely drawn, it is cut free of the ring — otherwise
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
		t.Error("the badge is not cut free of the ring; the two will merge")
	}
}

func TestTrayIconGlyphColour(t *testing.T) {
	t.Parallel()
	const size = 32
	dark := decodeIcon(t, trayIconOK, size, false)
	light := decodeIcon(t, trayIconOK, size, true)

	r, g, b, a := dark.At(size/2, size/2).RGBA()
	if a == 0 || r != 0 || g != 0 || b != 0 {
		t.Errorf("light-background glyph is not black: %d %d %d %d", r, g, b, a)
	}
	r, g, b, a = light.At(size/2, size/2).RGBA()
	if a == 0 || r != a || g != a || b != a {
		t.Errorf("dark-background glyph is not white: %d %d %d %d", r, g, b, a)
	}
}

func TestTrayIconRejectsAnEmptyCanvas(t *testing.T) {
	t.Parallel()
	if _, err := trayIconPNG(trayIconOK, 0, false); err == nil {
		t.Fatal("a zero-sized icon was accepted; it would install a blank image")
	}
}
