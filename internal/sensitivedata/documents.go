package sensitivedata

import "strings"

const (
	cpfDigits   = 11
	cnpjDigits  = 14
	cardMin     = 13
	cardMax     = 19
	cardGroups  = 5
	cpfMask     = "###.###.###-##"
	cnpjMask    = "##.###.###/####-##"
	numberGlues = ".-/"
)

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isAlnum(c byte) bool { return isDigit(c) || isLetter(c) }

// numberStart reports whether a numeric token may begin at i. A digit, a letter or an
// underscore before it means the number is part of a longer identifier; a separator
// glued to a previous digit means it continues a longer number.
func numberStart(s string, i int) bool {
	if i == 0 {
		return true
	}
	previous := s[i-1]
	if isAlnum(previous) || previous == '_' {
		return false
	}
	return !(strings.IndexByte(numberGlues, previous) >= 0 && i >= 2 && isDigit(s[i-2]))
}

// numberEnd is the mirror of numberStart for the byte after a token.
func numberEnd(s string, j int) bool {
	if j >= len(s) {
		return true
	}
	next := s[j]
	if isAlnum(next) || next == '_' {
		return false
	}
	return !(strings.IndexByte(numberGlues, next) >= 0 && j+1 < len(s) && isDigit(s[j+1]))
}

func allDigits(s string) bool {
	for i := range len(s) {
		if !isDigit(s[i]) {
			return false
		}
	}
	return s != ""
}

func repeatedDigits(s string) bool { return strings.Count(s, s[:1]) == len(s) }

// unmask returns the digits of value when its separators sit exactly where the mask
// puts them.
func unmask(value, mask string) (string, bool) {
	if len(value) != len(mask) {
		return "", false
	}
	digits := make([]byte, 0, len(mask))
	for i := range len(mask) {
		switch {
		case mask[i] == '#':
			if !isDigit(value[i]) {
				return "", false
			}
			digits = append(digits, value[i])
		case value[i] != mask[i]:
			return "", false
		}
	}
	return string(digits), true
}

// checkDigit is the module 11 rule shared by CPF and CNPJ, the same arithmetic the
// user identity validator applies: remainders 0 and 1 give zero.
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

func cpfWeight(position, length int) int { return length + 1 - position }

func cnpjWeight(position, length int) int { return (length-1-position)%8 + 2 }

func validCPF(digits string) bool {
	if len(digits) != cpfDigits || !allDigits(digits) || repeatedDigits(digits) {
		return false
	}
	return checkDigit(digits[:9], cpfWeight) == digits[9] && checkDigit(digits[:10], cpfWeight) == digits[10]
}

// validCNPJ covers the numeric form only. The alphanumeric CNPJ needs its own fixtures
// before it can be announced as covered.
func validCNPJ(digits string) bool {
	if len(digits) != cnpjDigits || !allDigits(digits) || repeatedDigits(digits) {
		return false
	}
	return checkDigit(digits[:12], cnpjWeight) == digits[12] && checkDigit(digits[:13], cnpjWeight) == digits[13]
}

func validLuhn(digits string) bool {
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// documentAt tries the fixed shapes of CPF and CNPJ at a token start.
func documentAt(s string, i int, wantCPF, wantCNPJ bool, emit func(start, end int, c Category, kind string) error) error {
	try := func(length int, mask string, c Category, valid func(string) bool) error {
		end := i + length
		if end <= len(s) && allDigits(s[i:end]) && numberEnd(s, end) && valid(s[i:end]) {
			return emit(i, end, c, c.String())
		}
		end = i + len(mask)
		if end > len(s) || !numberEnd(s, end) {
			return nil
		}
		if digits, ok := unmask(s[i:end], mask); ok && valid(digits) {
			return emit(i, end, c, c.String())
		}
		return nil
	}
	if wantCPF {
		if err := try(cpfDigits, cpfMask, CPF, validCPF); err != nil {
			return err
		}
	}
	if wantCNPJ {
		return try(cnpjDigits, cnpjMask, CNPJ, validCNPJ)
	}
	return nil
}

// cardAt reads 13 to 19 digits, contiguous or in groups split by one kind of
// separator. Grouped numbers may be followed by more groups, such as an expiry date,
// so the longest prefix that ends on a group and passes Luhn wins.
func cardAt(s string, i int, emit func(start, end int, c Category, kind string) error) error {
	j := i
	for j < len(s) && isDigit(s[j]) && j-i <= cardMax {
		j++
	}
	first := j - i
	if first >= cardMin {
		if first <= cardMax && numberEnd(s, j) {
			digits := s[i:j]
			if !repeatedDigits(digits) && validLuhn(digits) {
				return emit(i, j, Card, "payment_card")
			}
		}
		return nil
	}
	if first != 4 || j >= len(s) || (s[j] != ' ' && s[j] != '-') {
		return nil
	}
	separator := s[j]
	digits := make([]byte, 0, cardMax)
	digits = append(digits, s[i:j]...)
	type boundary struct{ end, count int }
	var ends []boundary
	for groups := 1; groups < cardGroups && j < len(s) && s[j] == separator; groups++ {
		k := j + 1
		for k < len(s) && isDigit(s[k]) && len(digits) <= cardMax {
			digits = append(digits, s[k])
			k++
		}
		size := k - j - 1
		if size == 0 || len(digits) > cardMax {
			break
		}
		j = k
		ends = append(ends, boundary{j, len(digits)})
		if j < len(s) && isDigit(s[j]) {
			break
		}
	}
	for n := len(ends) - 1; n >= 0; n-- {
		end, count := ends[n].end, ends[n].count
		if count < cardMin {
			break
		}
		closed := numberEnd(s, end) || (end < len(s) && s[end] == separator)
		if end < len(s) && isDigit(s[end]) {
			closed = false
		}
		value := string(digits[:count])
		if closed && !repeatedDigits(value) && validLuhn(value) {
			return emit(i, end, Card, "payment_card")
		}
	}
	return nil
}
