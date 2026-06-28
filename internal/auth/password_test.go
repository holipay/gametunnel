package auth

import "testing"

func TestCheckPasswordStrength(t *testing.T) {
	tests := []struct {
		password    string
		expectLevel string
		minWarnings int
	}{
		{"", "none", 0},
		{"a", "weak", 1},
		{"1234", "weak", 2},
		{"password", "weak", 2},
		{"hello", "weak", 0},
		{"abcdef", "weak", 0},
		{"abcdefgh", "moderate", 0},
		{"MyStr0ng!Pass", "strong", 0},
	}
	for _, tt := range tests {
		level, warnings := CheckPasswordStrength(tt.password)
		if level != tt.expectLevel {
			t.Errorf("CheckPasswordStrength(%q): level = %q, want %q", tt.password, level, tt.expectLevel)
		}
		if len(warnings) < tt.minWarnings {
			t.Errorf("CheckPasswordStrength(%q): %d warnings, want >= %d", tt.password, len(warnings), tt.minWarnings)
		}
	}
}
