package auth

import "strings"

// PasswordStrength returns a strength level and warnings for a given password.
// Empty password returns "none" with no warnings (valid: no auth).
func CheckPasswordStrength(password string) (level string, warnings []string) {
	if password == "" {
		return "none", nil
	}

	if len(password) < 4 {
		warnings = append(warnings, "password is very short (< 4 characters)")
	} else if len(password) < 8 {
		warnings = append(warnings, "consider using 8+ characters")
	}

	lower := strings.ToLower(password)
	weak := []string{"1234", "password", "admin", "test", "abc", "1111", "pass", "root", "default"}
	isWeakWord := false
	for _, w := range weak {
		if lower == w {
			warnings = append(warnings, "password is commonly used and weak")
			isWeakWord = true
			break
		}
	}

	switch {
	case len(password) >= 12 && !isWeakWord && len(warnings) == 0:
		level = "strong"
	case len(password) >= 8 && !isWeakWord:
		level = "moderate"
	default:
		level = "weak"
	}
	return level, warnings
}
