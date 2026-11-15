package sensitivedata

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

const (
	keyBodyMin       = 16
	keyBodyMax       = 256
	awsBody          = 16
	ghpBody          = 36
	githubPATMin     = 20
	githubPATMax     = 255
	pemHeaderMaxSpan = 64
)

var privateKeyLabels = []string{"PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY", "ENCRYPTED PRIVATE KEY"}

func isKeyByte(c byte) bool { return isAlnum(c) || c == '_' || c == '-' }

func isBase64URL(c byte) bool { return isAlnum(c) || c == '_' || c == '-' }

// secretScan finds the secret families of one string. Every family searches its
// prefix with strings.Index and resumes after what it read, so a run of repeated
// prefixes is read once. A candidate longer than the limit is a scan failure: it
// cannot be dismissed as harmless without reading it all.
type secretScan struct {
	r *run
	s string
}

func (x *secretScan) all() error {
	for _, family := range []func() error{x.apiKeys, x.aws, x.githubClassic, x.githubFineGrained, x.jwt, x.pem} {
		x.r.resetTick()
		if err := family(); err != nil {
			return err
		}
	}
	return nil
}

// tokenRun returns the end of the run of bytes accepted by allowed from p, failing
// when the run exceeds the candidate limit.
func (x *secretScan) tokenRun(p int, allowed func(byte) bool) (int, error) {
	end := p
	for end < len(x.s) && allowed(x.s[end]) {
		end++
		if end-p > x.r.limits.MaxSecretBytes {
			return 0, errCapacity
		}
	}
	return end, nil
}

func (x *secretScan) apiKeys() error {
	s := x.s
	for from := 0; from < len(s); {
		k := strings.Index(s[from:], "sk-")
		if k < 0 {
			return nil
		}
		at := from + k
		if err := x.r.tick(at); err != nil {
			return err
		}
		if at > 0 && isKeyByte(s[at-1]) {
			from = at + 1
			continue
		}
		end, err := x.tokenRun(at+3, isKeyByte)
		if err != nil {
			return err
		}
		body, kind := end-at-3, KindOpenAIAPIKey
		if strings.HasPrefix(s[at+3:end], "ant-") {
			body, kind = end-at-7, KindAnthropicAPIKey
		}
		if body >= keyBodyMin && body <= keyBodyMax {
			if err := x.r.emit(at, end, Secret, kind); err != nil {
				return err
			}
		}
		from = max(end, at+1)
	}
	return nil
}

func (x *secretScan) aws() error {
	s := x.s
	for from := 0; from < len(s); {
		k := strings.Index(s[from:], "AKIA")
		if k < 0 {
			return nil
		}
		at := from + k
		from = at + 1
		if err := x.r.tick(at); err != nil {
			return err
		}
		end := at + 4 + awsBody
		if (at > 0 && isAlnum(s[at-1])) || end > len(s) || (end < len(s) && isAlnum(s[end])) {
			continue
		}
		valid := true
		for _, c := range []byte(s[at+4 : end]) {
			if !isDigit(c) && (c < 'A' || c > 'Z') {
				valid = false
				break
			}
		}
		if valid {
			if err := x.r.emit(at, end, Secret, KindAWSAccessKeyID); err != nil {
				return err
			}
		}
	}
	return nil
}

func isPATByte(c byte) bool { return isAlnum(c) || c == '_' }

func (x *secretScan) githubClassic() error {
	s := x.s
	for from := 0; from < len(s); {
		k := strings.Index(s[from:], "ghp_")
		if k < 0 {
			return nil
		}
		at := from + k
		if err := x.r.tick(at); err != nil {
			return err
		}
		if at > 0 && isPATByte(s[at-1]) {
			from = at + 1
			continue
		}
		end, err := x.tokenRun(at+4, isPATByte)
		if err != nil {
			return err
		}
		if end-at-4 == ghpBody && !strings.Contains(s[at+4:end], "_") {
			if err := x.r.emit(at, end, Secret, KindGitHubToken); err != nil {
				return err
			}
		}
		from = max(end, at+1)
	}
	return nil
}

func (x *secretScan) githubFineGrained() error {
	s := x.s
	for from := 0; from < len(s); {
		k := strings.Index(s[from:], "github_pat_")
		if k < 0 {
			return nil
		}
		at := from + k
		if err := x.r.tick(at); err != nil {
			return err
		}
		if at > 0 && isPATByte(s[at-1]) {
			from = at + 1
			continue
		}
		end, err := x.tokenRun(at+11, isPATByte)
		if err != nil {
			return err
		}
		if body := end - at - 11; body >= githubPATMin && body <= githubPATMax {
			if err := x.r.emit(at, end, Secret, KindGitHubToken); err != nil {
				return err
			}
		}
		from = max(end, at+1)
	}
	return nil
}

func isJWTByte(c byte) bool { return isBase64URL(c) || c == '.' }

// jwt checks the structure only: three non-empty base64url segments whose first two
// decode to JSON objects, the first naming an algorithm. The signature is not
// verified and the token may well be expired.
func (x *secretScan) jwt() error {
	s := x.s
	for from := 0; from < len(s); {
		k := strings.Index(s[from:], "eyJ")
		if k < 0 {
			return nil
		}
		at := from + k
		if err := x.r.tick(at); err != nil {
			return err
		}
		if at > 0 && isJWTByte(s[at-1]) {
			from = at + 1
			continue
		}
		end, err := x.tokenRun(at, isJWTByte)
		if err != nil {
			return err
		}
		for end > at && s[end-1] == '.' {
			end--
		}
		if validJWT(s[at:end]) {
			if err := x.r.emit(at, end, Secret, KindJWT); err != nil {
				return err
			}
		}
		from = max(end, at+1)
	}
	return nil
}

func validJWT(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return false
	}
	for _, part := range parts {
		for i := range len(part) {
			if !isBase64URL(part[i]) {
				return false
			}
		}
	}
	var header struct {
		Alg *string `json:"alg"`
	}
	if !decodeObject(parts[0], &header) || header.Alg == nil {
		return false
	}
	var payload map[string]json.RawMessage
	return decodeObject(parts[1], &payload)
}

func decodeObject(segment string, target any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	return json.Unmarshal(raw, target) == nil
}

// pem pairs each private key header with the next footer of the same label. Footer
// positions are listed once per label, so a text full of headers without footers is
// still read in linear time.
func (x *secretScan) pem() error {
	s := x.s
	if !strings.Contains(s, "-----BEGIN ") {
		return nil
	}
	footers := make(map[string][]int, len(privateKeyLabels))
	for _, label := range privateKeyLabels {
		footer := "-----END " + label + "-----"
		for from := 0; ; {
			k := strings.Index(s[from:], footer)
			if k < 0 {
				break
			}
			footers[label] = append(footers[label], from+k)
			from += k + len(footer)
			if err := x.r.tick(from); err != nil {
				return err
			}
		}
	}
	next := make(map[string]int, len(privateKeyLabels))
	for from := 0; from < len(s); {
		k := strings.Index(s[from:], "-----BEGIN ")
		if k < 0 {
			return nil
		}
		at := from + k
		from = at + 1
		if err := x.r.tick(at); err != nil {
			return err
		}
		labelStart := at + len("-----BEGIN ")
		window := s[labelStart:min(len(s), labelStart+pemHeaderMaxSpan)]
		closing := strings.Index(window, "-----")
		if closing < 0 {
			continue
		}
		label := window[:closing]
		positions, known := footers[label]
		if !known {
			continue
		}
		bodyStart := labelStart + closing + len("-----")
		i := next[label]
		for i < len(positions) && positions[i] < bodyStart {
			i++
		}
		next[label] = i
		if i == len(positions) {
			continue
		}
		footerAt := positions[i]
		end := footerAt + len("-----END "+label+"-----")
		if end-at > x.r.limits.MaxSecretBytes {
			return errCapacity
		}
		if validPEMBody(s[bodyStart:footerAt]) {
			if err := x.r.emit(at, end, Secret, KindPrivateKey); err != nil {
				return err
			}
			from = end
		}
	}
	return nil
}

// validPEMBody drops the RFC 1421 header lines of legacy encrypted keys and requires
// the rest to be non-empty standard base64.
func validPEMBody(body string) bool {
	var encoded strings.Builder
	for line := range strings.Lines(body) {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, ":") {
			continue
		}
		encoded.WriteString(trimmed)
	}
	if encoded.Len() == 0 {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(encoded.String())
	return err == nil
}
