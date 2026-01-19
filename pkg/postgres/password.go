package postgres

import (
	"crypto/rand"
	"math/big"
)

const (
	DefaultPasswordLength = 16
	MinPasswordLength     = 12
	MaxPasswordLength     = 64

	lowerChars   = "abcdefghijklmnopqrstuvwxyz"
	upperChars   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	digitChars   = "0123456789"
	specialChars = "._-,^"

	minLower   = 3
	minUpper   = 3
	minDigit   = 3
	minSpecial = 3
)

func GeneratePassword(length int) (string, error) {
	switch {
	case length == 0:
		length = DefaultPasswordLength
	case length < MinPasswordLength:
		length = MinPasswordLength
	case length > MaxPasswordLength:
		length = MaxPasswordLength
	}

	result := make([]byte, length)
	pos := 0

	// Guarantee minimum of each character type
	if err := fillFromCharset(result, &pos, lowerChars, minLower); err != nil {
		return "", err
	}
	if err := fillFromCharset(result, &pos, upperChars, minUpper); err != nil {
		return "", err
	}
	if err := fillFromCharset(result, &pos, digitChars, minDigit); err != nil {
		return "", err
	}
	if err := fillFromCharset(result, &pos, specialChars, minSpecial); err != nil {
		return "", err
	}

	// Fill remaining positions with random characters from all charsets
	allChars := lowerChars + upperChars + digitChars + specialChars
	if err := fillFromCharset(result, &pos, allChars, length-pos); err != nil {
		return "", err
	}

	// Shuffle to avoid predictable pattern
	if err := shuffle(result); err != nil {
		return "", err
	}

	return string(result), nil
}

func fillFromCharset(result []byte, pos *int, charset string, count int) error {
	charsetLen := big.NewInt(int64(len(charset)))
	for i := 0; i < count && *pos < len(result); i++ {
		idx, err := rand.Int(rand.Reader, charsetLen)
		if err != nil {
			return err
		}
		result[*pos] = charset[idx.Int64()]
		*pos++
	}
	return nil
}

func shuffle(data []byte) error {
	n := len(data)
	for i := n - 1; i > 0; i-- {
		jBig, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return err
		}
		j := int(jBig.Int64())
		data[i], data[j] = data[j], data[i]
	}
	return nil
}
