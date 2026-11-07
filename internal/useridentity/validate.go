package useridentity

import (
	"strings"
	"unicode"
)

const (
	maxIDLength = 128

	cpfDigits  = 11
	cnpjLength = 14
)

// ValidateID accepts opaque identifiers only. Email and documents are classified
// first because their errors tell the developer what to fix; the character rule would
// otherwise answer them with a generic message. It never normalizes the value: what
// passes here is hashed exactly as it was sent.
func ValidateID(id string) error {
	if id == "" || len(id) > maxIDLength {
		return ErrUserInvalid
	}
	if looksLikeEmail(id) {
		return ErrUserEmail
	}
	if isCPF(id) || isCNPJ(id) {
		return ErrUserDocument
	}
	for i := range len(id) {
		if !allowedByte(id[i]) {
			return ErrUserInvalid
		}
	}
	return nil
}

func allowedByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return c == '_' || c == '.' || c == ':' || c == '-'
}

// looksLikeEmail is a classification of the error, not an address validator.
func looksLikeEmail(id string) bool {
	at := strings.IndexByte(id, '@')
	if at <= 0 || at == len(id)-1 {
		return false
	}
	return !strings.ContainsFunc(id, unicode.IsSpace)
}

// stripMask removes the separators only when they sit exactly where the mask puts
// them, so a longer identifier that merely contains punctuation stays an identifier.
func stripMask(value, mask string) (string, bool) {
	if len(value) != len(mask) {
		return "", false
	}
	var out strings.Builder
	for i := range len(mask) {
		switch {
		case mask[i] == '#':
			out.WriteByte(value[i])
		case value[i] != mask[i]:
			return "", false
		}
	}
	return out.String(), true
}

func isCPF(id string) bool {
	digits := id
	if len(id) != cpfDigits {
		stripped, ok := stripMask(id, "###.###.###-##")
		if !ok {
			return false
		}
		digits = stripped
	}
	if !allDigits(digits) || repeated(digits) {
		return false
	}
	return checkDigit(digits[:9], cpfWeight) == digits[9] && checkDigit(digits[:10], cpfWeight) == digits[10]
}

// cpfWeight counts down to 2 from the position of the digit being calculated.
func cpfWeight(position, length int) int { return length + 1 - position }

// isCNPJ covers both the numeric form and the alphanumeric base: each base character
// is worth its ASCII code minus 48, and the last two positions are always digits.
// Letters are uppercased only in this temporary copy, never in what gets hashed.
func isCNPJ(id string) bool {
	value := id
	if len(id) != cnpjLength {
		stripped, ok := stripMask(id, "##.###.###/####-##")
		if !ok {
			return false
		}
		value = stripped
	}
	value = strings.ToUpper(value)
	if len(value) != cnpjLength {
		return false
	}
	for i := range cnpjLength - 2 {
		if !isCNPJBase(value[i]) {
			return false
		}
	}
	if !allDigits(value[cnpjLength-2:]) || repeated(value) {
		return false
	}
	return checkDigit(value[:12], cnpjWeight) == value[12] && checkDigit(value[:13], cnpjWeight) == value[13]
}

func isCNPJBase(c byte) bool { return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') }

// cnpjWeight cycles from 2 up to 9, counted from the right end of the input.
func cnpjWeight(position, length int) int {
	return (length-1-position)%8 + 2
}

// checkDigit is the module 11 rule shared by CPF and CNPJ: a remainder of 0 or 1 gives
// zero, any other gives 11 minus the remainder.
func checkDigit(base string, weight func(position, length int) int) byte {
	sum := 0
	for i := range len(base) {
		sum += int(base[i]-'0') * weight(i, len(base))
	}
	remainder := sum % 11
	if remainder < 2 {
		return '0'
	}
	return byte('0' + 11 - remainder)
}

func allDigits(value string) bool {
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func repeated(value string) bool {
	return strings.Count(value, value[:1]) == len(value)
}
