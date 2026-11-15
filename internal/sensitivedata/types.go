// Package sensitivedata finds personal data and secrets in request text, and turns
// what it finds into counts, replacements or a refusal. It never imports the proxy,
// metering or storage: callers hand it decoded strings and receive decisions plus a
// summary that holds categories and counts only.
package sensitivedata

import (
	"errors"
	"slices"
)

// Category is a kind of sensitive data. The declaration order is the order of every
// summary.
type Category uint8

const (
	CPF Category = iota
	CNPJ
	Card
	Email
	Phone
	Pix
	Secret
	categoryCount
)

var categoryNames = [categoryCount]string{"cpf", "cnpj", "card", "email", "phone", "pix", "secret"}

// Categories lists every category in summary order.
func Categories() []Category {
	return []Category{CPF, CNPJ, Card, Email, Phone, Pix, Secret}
}

func (c Category) String() string {
	if c >= categoryCount {
		return "unknown"
	}
	return categoryNames[c]
}

func (c Category) valid() bool { return c < categoryCount }

// ParseCategory reads the configuration and summary spelling of a category.
func ParseCategory(name string) (Category, bool) {
	for i, known := range categoryNames {
		if known == name {
			return Category(i), true
		}
	}
	return 0, false
}

// MarshalText writes the category name.
func (c Category) MarshalText() ([]byte, error) {
	if !c.valid() {
		return nil, errors.New("invalid data type")
	}
	return []byte(c.String()), nil
}

// UnmarshalText accepts only a known category name.
func (c *Category) UnmarshalText(text []byte) error {
	parsed, ok := ParseCategory(string(text))
	if !ok {
		return errors.New("invalid data type")
	}
	*c = parsed
	return nil
}

// precedence is the order used to pick the message of a refusal and to break ties
// between overlapping replacements: a secret is always the most serious finding.
var precedence = [categoryCount]int{Secret: 0, CPF: 1, CNPJ: 2, Card: 3, Email: 4, Phone: 5, Pix: 6}

// Mode is the policy of one category.
type Mode uint8

const (
	ModeOff Mode = iota
	ModeDetect
	ModeRedact
	ModeBlock
)

var modeNames = [...]string{"off", "detect", "redact", "block"}

func (m Mode) String() string {
	if int(m) >= len(modeNames) {
		return "unknown"
	}
	return modeNames[m]
}

// ParseMode reads the configuration spelling of a mode.
func ParseMode(name string) (Mode, bool) {
	for i, known := range modeNames {
		if known == name {
			return Mode(i), true
		}
	}
	return 0, false
}

// MarshalText writes the mode name.
func (m Mode) MarshalText() ([]byte, error) {
	if int(m) >= len(modeNames) {
		return nil, errors.New("invalid action")
	}
	return []byte(m.String()), nil
}

// UnmarshalText accepts only a known mode name.
func (m *Mode) UnmarshalText(text []byte) error {
	parsed, ok := ParseMode(string(text))
	if !ok {
		return errors.New("invalid action")
	}
	*m = parsed
	return nil
}

// Policy is the detection configuration of one app, fixed for one request.
type Policy struct {
	Modes     [categoryCount]Mode
	Rehydrate bool
}

// Mode returns the mode of a category.
func (p Policy) Mode(c Category) Mode {
	if !c.valid() {
		return ModeOff
	}
	return p.Modes[c]
}

// Set changes the mode of a category.
func (p *Policy) Set(c Category, m Mode) {
	if c.valid() {
		p.Modes[c] = m
	}
}

// Active reports whether any category is inspected at all.
func (p Policy) Active() bool {
	return slices.ContainsFunc(p.Modes[:], func(m Mode) bool { return m != ModeOff })
}

// Protective reports whether some category changes or refuses the request. A
// protective policy fails closed: an incomplete scan cannot forward the request.
func (p Policy) Protective() bool {
	return slices.ContainsFunc(p.Modes[:], func(m Mode) bool { return m == ModeRedact || m == ModeBlock })
}

// Secret subtypes. They are the only free-looking words a summary may carry, and
// they come from this allowlist alone.
const (
	KindAnthropicAPIKey = "anthropic_api_key"
	KindOpenAIAPIKey    = "openai_api_key"
	KindAWSAccessKeyID  = "aws_access_key_id"
	KindGitHubToken     = "github_token"
	KindJWT             = "jwt"
	KindPrivateKey      = "private_key"
)

var secretKinds = []string{KindAnthropicAPIKey, KindAWSAccessKeyID, KindGitHubToken, KindJWT, KindOpenAIAPIKey, KindPrivateKey}

// ValidSecretKind reports whether a subtype belongs to the allowlist.
func ValidSecretKind(kind string) bool { return slices.Contains(secretKinds, kind) }

// MaxCount bounds the count of one category, which can never exceed the
// candidates a request may produce.
const MaxCount = 4096

// Detection is the safe summary of one category: what was seen, how often and the
// policy applied. It never holds a value, a position or anything derived from one.
type Detection struct {
	DataType    Category `json:"data_type"`
	Count       int64    `json:"count"`
	Action      Mode     `json:"action"`
	SecretKinds []string `json:"secret_kinds,omitempty"`
}

// Summary lists the categories found in one request, in category order.
type Summary []Detection

var errInvalidSummary = errors.New("invalid sensitive data summary")

// Validate holds a summary to its contract: unique categories in order, positive
// bounded counts, an action other than off, and secret kinds only on secrets.
func (s Summary) Validate() error {
	if len(s) > int(categoryCount) {
		return errInvalidSummary
	}
	for i, d := range s {
		if !d.DataType.valid() || d.Count < 1 || d.Count > MaxCount {
			return errInvalidSummary
		}
		if d.Action != ModeDetect && d.Action != ModeRedact && d.Action != ModeBlock {
			return errInvalidSummary
		}
		if i > 0 && s[i-1].DataType >= d.DataType {
			return errInvalidSummary
		}
		if len(d.SecretKinds) > 0 && d.DataType != Secret {
			return errInvalidSummary
		}
		for j, kind := range d.SecretKinds {
			if !ValidSecretKind(kind) || (j > 0 && d.SecretKinds[j-1] >= kind) {
				return errInvalidSummary
			}
		}
	}
	return nil
}

// Clone copies the summary deeply.
func (s Summary) Clone() Summary {
	if s == nil {
		return nil
	}
	out := make(Summary, len(s))
	for i, d := range s {
		d.SecretKinds = slices.Clone(d.SecretKinds)
		out[i] = d
	}
	return out
}
