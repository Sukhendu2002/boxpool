package preflight

import "testing"

func TestMajorAtLeast(t *testing.T) {
	cases := []struct {
		v    string
		min  int
		want bool
	}{
		{"14.0", 14, true},
		{"15.3.1", 14, true},
		{"13.6", 14, false},
		{"26", 14, true},
		{"", 14, false},
		{"abc", 14, false},
	}
	for _, c := range cases {
		if got := MajorAtLeast(c.v, c.min); got != c.want {
			t.Errorf("MajorAtLeast(%q, %d) = %v, want %v", c.v, c.min, got, c.want)
		}
	}
}

func TestHasVirtFlag(t *testing.T) {
	if !hasVirtFlag("processor : 0\nflags : fpu vme vmx sse\n") {
		t.Error("expected vmx to be found")
	}
	if hasVirtFlag("flags : fpu sse\n") {
		t.Error("expected no virt flag")
	}
}
