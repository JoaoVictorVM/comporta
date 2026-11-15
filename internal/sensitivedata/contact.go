package sensitivedata

import "strings"

const (
	maxLocalPart  = 64
	maxDomain     = 253
	maxLabel      = 63
	uuidLength    = 36
	phoneTableRev = "anatel-ddd-2024"
)

// PhoneTableRevision names the area code table the phone detector validates against.
const PhoneTableRevision = phoneTableRev

// areaCodes are the Brazilian two digit area codes in use. A number with any other
// prefix is not reported, which trades some recall for fewer false positives.
var areaCodes = func() [100]bool {
	var table [100]bool
	for _, code := range []int{
		11, 12, 13, 14, 15, 16, 17, 18, 19, 21, 22, 24, 27, 28, 31, 32, 33, 34, 35, 37, 38,
		41, 42, 43, 44, 45, 46, 47, 48, 49, 51, 53, 54, 55, 61, 62, 63, 64, 65, 66, 67, 68, 69,
		71, 73, 74, 75, 77, 79, 81, 82, 83, 84, 85, 86, 87, 88, 89, 91, 92, 93, 94, 95, 96, 97, 98, 99,
	} {
		table[code] = true
	}
	return table
}()

// isLocalByte accepts the practical subset of an address local part. Rarer symbols
// that RFC 5322 allows are left out so that query strings and assignments around an
// address do not become part of the match.
func isLocalByte(c byte) bool {
	return isAlnum(c) || c == '.' || c == '_' || c == '%' || c == '+' || c == '-'
}

func isDomainByte(c byte) bool { return isAlnum(c) || c == '.' || c == '-' }

// emailAt reads the address around the @ at position at. Each @ reads only the
// bytes between it and its neighbours, so the whole pass stays linear.
func emailAt(s string, at int) (start, end int, ok bool) {
	start = at
	for start > 0 && isLocalByte(s[start-1]) {
		start--
		if at-start > maxLocalPart {
			return 0, 0, false
		}
	}
	local := s[start:at]
	if local == "" || local[0] == '.' || local[len(local)-1] == '.' || strings.Contains(local, "..") {
		return 0, 0, false
	}
	end = at + 1
	for end < len(s) && isDomainByte(s[end]) {
		end++
		if end-at-1 > maxDomain+1 {
			return 0, 0, false
		}
	}
	for end > at+1 && (s[end-1] == '.' || s[end-1] == '-') {
		end--
	}
	domain := s[at+1 : end]
	if len(domain) > maxDomain || !validDomain(domain) {
		return 0, 0, false
	}
	return start, end, true
}

func validDomain(domain string) bool {
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > maxLabel || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	tld := labels[len(labels)-1]
	if len(tld) < 2 {
		return false
	}
	for i := range len(tld) {
		if !isLetter(tld[i]) {
			return false
		}
	}
	return true
}

// phoneAt reads a Brazilian number: optional country code 55 (with or without +), a
// valid area code, optionally in parentheses, and a subscriber of 9 digits starting
// with 9 or 8 digits starting with 2 to 5, with the usual separators.
func phoneAt(s string, i int) (int, bool) {
	if s[i] == '+' {
		if !strings.HasPrefix(s[i+1:], "55") {
			return 0, false
		}
		return phoneRest(s, optionalSeparator(s, i+3))
	}
	if strings.HasPrefix(s[i:], "55") {
		if end, ok := phoneRest(s, optionalSeparator(s, i+2)); ok {
			return end, true
		}
	}
	return phoneRest(s, i)
}

func optionalSeparator(s string, p int) int {
	if p < len(s) && (s[p] == ' ' || s[p] == '-') {
		return p + 1
	}
	return p
}

func phoneRest(s string, p int) (int, bool) {
	var area string
	switch {
	case p+4 <= len(s) && s[p] == '(' && s[p+3] == ')':
		area = s[p+1 : p+3]
		p += 4
		if p < len(s) && s[p] == ' ' {
			p++
		}
	case p+2 <= len(s):
		area = s[p : p+2]
		p += 2
		if p < len(s) && s[p] == ' ' {
			p++
		}
	default:
		return 0, false
	}
	if !allDigits(area) || !areaCodes[int(area[0]-'0')*10+int(area[1]-'0')] {
		return 0, false
	}
	for _, size := range [2]int{9, 8} {
		if end, ok := subscriber(s, p, size); ok {
			return end, true
		}
	}
	return 0, false
}

func subscriber(s string, p, size int) (int, bool) {
	if p >= len(s) || !isDigit(s[p]) {
		return 0, false
	}
	if size == 9 && s[p] != '9' {
		return 0, false
	}
	if size == 8 && (s[p] < '2' || s[p] > '5') {
		return 0, false
	}
	head := size - 4
	if end := p + size; end <= len(s) && allDigits(s[p:end]) && numberEnd(s, end) {
		return end, true
	}
	split := p + head
	end := split + 5
	if end <= len(s) && allDigits(s[p:split]) && (s[split] == '-' || s[split] == ' ') && allDigits(s[split+1:end]) && numberEnd(s, end) {
		return end, true
	}
	return 0, false
}

func isHex(c byte) bool { return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') }

// uuidAt checks a canonical version 4 UUID starting at i. It is reported as a PIX
// random key candidate: nothing in the text proves the UUID is registered as one.
func uuidAt(s string, i int) bool {
	if i < 0 || i+uuidLength > len(s) {
		return false
	}
	if i > 0 && (isAlnum(s[i-1]) || s[i-1] == '-' || s[i-1] == '_') {
		return false
	}
	if end := i + uuidLength; end < len(s) && (isAlnum(s[end]) || s[end] == '-' || s[end] == '_') {
		return false
	}
	for k := range uuidLength {
		c := s[i+k]
		switch k {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHex(c) {
				return false
			}
		}
	}
	variant := s[i+19]
	return s[i+14] == '4' && (variant == '8' || variant == '9' || variant == 'a' || variant == 'b' || variant == 'A' || variant == 'B')
}
