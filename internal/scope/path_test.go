package scope

import "testing"

func TestNormalizePath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"/a/b", "/a/b"},
		{"/a/b/", "/a/b"},
		{"/a/b///", "/a/b"},
		{"/a//b", "/a/b"},
		{"/", "/"},
		{"///", "/"},
		// Windows drive form: slashes unified, lowercased, trailing stripped.
		{`C:\Users\Dev\Proj`, "c:/users/dev/proj"},
		{`C:\Users\Dev\Proj\`, "c:/users/dev/proj"},
		{"C:/Users/Dev", "c:/users/dev"},
		{`c:\`, "c:"},
		// Any backslash marks the path as Windows-form (lowercase).
		{`\Some\Share`, "/some/share"},
		// UNC: single leading double slash preserved.
		{`\\Host\Share\Dir`, "//host/share/dir"},
		{`\\Host\Share\`, "//host/share"},
		// POSIX paths keep their case.
		{"/A/B/c", "/A/B/c"},
		// Relative paths pass through as strings (no canonicalization).
		{"a/b/./c", "a/b/./c"},
		{"a/b/../c", "a/b/../c"},
	}
	for _, tc := range cases {
		if got := NormalizePath(tc.in); got != tc.want {
			t.Errorf("NormalizePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizePathIdempotent(t *testing.T) {
	for _, p := range []string{`C:\A\B\`, "/a//b/", `\\H\S\x`, "/", ""} {
		once := NormalizePath(p)
		if twice := NormalizePath(once); twice != once {
			t.Errorf("not idempotent: %q -> %q -> %q", p, once, twice)
		}
	}
}
