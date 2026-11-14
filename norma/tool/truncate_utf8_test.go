package tool

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// CJK output is 3 bytes per rune: a blind byte cut splits a rune and the
// invalid tail corrupts everything downstream that assumes UTF-8. Capture and
// truncate must always return valid UTF-8.
func TestCaptureTruncationIsValidUTF8(t *testing.T) {
	// 12000 CJK runes = 36000 bytes, over the 30000 default.
	s := strings.Repeat("渗透", 6000)
	for i := 0; i < 6000; i++ {
		s += "测"
	}
	got := Capture(nil, s)
	if !utf8.ValidString(got) {
		t.Fatal("Capture produced invalid UTF-8")
	}
	if len(got) > defaultMaxOutput+100 {
		t.Fatalf("Capture did not bound the output: %d bytes", len(got))
	}

	got = truncate(s, 5000)
	if !utf8.ValidString(got) {
		t.Fatal("truncate produced invalid UTF-8")
	}

	// A mixed multibyte/ASCII string cut right at a boundary between runes.
	mixed := strings.Repeat("ab", 100) + strings.Repeat("é", 500) // é is 2 bytes
	got = truncate(mixed, 300)
	if !utf8.ValidString(got) {
		t.Fatal("truncate on mixed ASCII/multibyte produced invalid UTF-8")
	}
}

// utf8Floor never extends the cut and never splits a rune.
func TestUTF8Floor(t *testing.T) {
	s := "aé中\U0001F600" // 1 + 2 + 3 + 4 bytes
	cases := map[int]int{ // cut position -> expected byte length
		0: 0,
		1: 1,  // after "a"
		2: 1,  // inside é -> back off to "a"
		3: 3,  // after é
		5: 3,  // inside 中 -> after é
		6: 6,  // after 中
		8: 6,  // inside the emoji -> after 中
		9: 6,  // inside the emoji again -> after 中
	}
	for in, want := range cases {
		if got := len(utf8Floor(s, in)); got != want {
			t.Errorf("utf8Floor(%d): got %d bytes, want %d", in, got, want)
		}
	}
	if !utf8.ValidString(utf8Floor(s, 8)) {
		t.Fatal("utf8Floor produced invalid UTF-8")
	}
}
