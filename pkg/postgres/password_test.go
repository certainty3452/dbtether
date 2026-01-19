package postgres

import (
	"strings"
	"testing"
	"unicode"
)

func TestGeneratePassword_DefaultLength(t *testing.T) {
	password, err := GeneratePassword(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(password) != DefaultPasswordLength {
		t.Errorf("expected length %d, got %d", DefaultPasswordLength, len(password))
	}
}

func TestGeneratePassword_CustomLength(t *testing.T) {
	tests := []struct {
		name   string
		length int
		want   int
	}{
		{"minimum", MinPasswordLength, MinPasswordLength},
		{"maximum", MaxPasswordLength, MaxPasswordLength},
		{"custom 20", 20, 20},
		{"custom 32", 32, 32},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			password, err := GeneratePassword(tt.length)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(password) != tt.want {
				t.Errorf("expected length %d, got %d", tt.want, len(password))
			}
		})
	}
}

func TestGeneratePassword_BelowMinimum(t *testing.T) {
	password, err := GeneratePassword(MinPasswordLength - 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(password) != MinPasswordLength {
		t.Errorf("expected length %d (minimum), got %d", MinPasswordLength, len(password))
	}
}

func TestGeneratePassword_AboveMaximum(t *testing.T) {
	password, err := GeneratePassword(MaxPasswordLength + 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(password) != MaxPasswordLength {
		t.Errorf("expected length %d (maximum), got %d", MaxPasswordLength, len(password))
	}
}

func TestGeneratePassword_OnlySafeCharacters(t *testing.T) {
	allowedChars := lowerChars + upperChars + digitChars + specialChars

	for i := 0; i < 100; i++ {
		password, err := GeneratePassword(32)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		for _, r := range password {
			if !strings.ContainsRune(allowedChars, r) {
				t.Errorf("password contains disallowed character: %q in %q", r, password)
			}
		}
	}
}

func TestGeneratePassword_Uniqueness(t *testing.T) {
	passwords := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		password, err := GeneratePassword(16)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if passwords[password] {
			t.Errorf("duplicate password generated: %s", password)
		}
		passwords[password] = true
	}
}

func TestGeneratePassword_GuaranteesMinimumOfEachType(t *testing.T) {
	for i := 0; i < 100; i++ {
		password, err := GeneratePassword(DefaultPasswordLength)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var lowerCount, upperCount, digitCount, specialCount int
		for _, r := range password {
			switch {
			case unicode.IsLower(r):
				lowerCount++
			case unicode.IsUpper(r):
				upperCount++
			case unicode.IsDigit(r):
				digitCount++
			case strings.ContainsRune(specialChars, r):
				specialCount++
			}
		}

		if lowerCount < minLower {
			t.Errorf("password has %d lowercase chars, expected at least %d: %s", lowerCount, minLower, password)
		}
		if upperCount < minUpper {
			t.Errorf("password has %d uppercase chars, expected at least %d: %s", upperCount, minUpper, password)
		}
		if digitCount < minDigit {
			t.Errorf("password has %d digit chars, expected at least %d: %s", digitCount, minDigit, password)
		}
		if specialCount < minSpecial {
			t.Errorf("password has %d special chars, expected at least %d: %s", specialCount, minSpecial, password)
		}
	}
}

func TestGeneratePassword_SpecialCharsFromSafeSet(t *testing.T) {
	for i := 0; i < 100; i++ {
		password, err := GeneratePassword(32)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		for _, r := range password {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				if !strings.ContainsRune(specialChars, r) {
					t.Errorf("password contains unsafe special character: %q (allowed: %s)", r, specialChars)
				}
			}
		}
	}
}

func TestGeneratePassword_Shuffled(t *testing.T) {
	startsWithLower := 0
	for i := 0; i < 100; i++ {
		password, err := GeneratePassword(16)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if unicode.IsLower(rune(password[0])) {
			startsWithLower++
		}
	}

	// Without shuffling, first chars would always be lowercase (3 of them)
	// With shuffling, distribution should be more even
	// Allow some variance, but not all should start with lowercase
	if startsWithLower == 100 {
		t.Error("passwords not shuffled - all start with lowercase")
	}
}
