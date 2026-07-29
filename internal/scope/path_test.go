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

func TestPathIsWithin(t *testing.T) {
	cases := []struct {
		root, p string
		want    bool
	}{
		{"/a/proj", "/a/proj", true},
		{"/a/proj", "/a/proj/sub", true},
		{"/a/proj", "/a/proj/sub/deep", true},
		// The classic boundary case: /a/proj must not swallow /a/project.
		{"/a/proj", "/a/project", false},
		{"/a/proj", "/a/proj2", false},
		{"/a/proj", "/a", false},
		{"/a/proj", "/b/proj/x", false},
		{"/", "/anything", true},
		{"/", "/", true},
		// Windows form (both already normalized).
		{"c:/users/dev", "c:/users/dev/proj", true},
		{"c:/users/dev", "c:/users/developer", false},
		{"c:", "c:/users", true},
		{"//host/share", "//host/share/dir", true},
		{"//host/share", "//host/share2", false},
		// Ambiguity → false (fail direction: withhold the project override).
		{"", "/a", false},
		{"/a", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		if got := PathIsWithin(tc.root, tc.p); got != tc.want {
			t.Errorf("PathIsWithin(%q, %q) = %v, want %v", tc.root, tc.p, got, tc.want)
		}
	}
}
