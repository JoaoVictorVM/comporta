// Package testkit holds the synthetic prompts and the scanner of the privacy canary.
// Only tests import it, so none of it reaches the binary, which a canary test verifies.
package testkit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// MarkerEnv lets one run share a marker across the Go, SDK and container suites. Without
// it each process draws its own.
const MarkerEnv = "COMPORTA_PRIVACY_MARKER"

const syntheticCPF = "52998224725"

var (
	markerShape = regexp.MustCompile(`^cmpcanary[0-9a-f]{32}$`)
	markerOnce  = sync.OnceValue(loadMarker)
)

func loadMarker() string {
	if value := os.Getenv(MarkerEnv); value != "" {
		if !markerShape.MatchString(value) {
			panic(MarkerEnv + " must be cmpcanary followed by 32 lowercase hex characters")
		}
		return value
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic("no entropy for the privacy marker")
	}
	return "cmpcanary" + hex.EncodeToString(random[:])
}

// Marker is what every synthetic prompt carries.
func Marker() string { return markerOnce() }

// UserMarker is a valid opaque identifier, distinct from the prompt marker, so that a
// finding tells a leaked user apart from leaked content.
func UserMarker() string { return "cmpuser" + strings.TrimPrefix(Marker(), "cmpcanary") }

// EmailID is an identifier the gateway must refuse without echoing it.
func EmailID() string { return UserMarker() + "@mail.example" }

// SpacedID fails the character rule and must not be echoed either.
func SpacedID() string { return UserMarker() + " with space" }

// DocumentID is a synthetic CPF with valid check digits.
func DocumentID() string { return syntheticCPF }

// Prompt marks a prompt text.
func Prompt(text string) string { return text + " " + Marker() }

// AnthropicBody is a normal Messages request.
func AnthropicBody() string {
	return `{"model":"fake-claude","max_tokens":32,"messages":[{"role":"user","content":"` + Prompt("Ola") + `"}],"stream":false,"future_option":{"enabled":true}}`
}

// AnthropicStreamBody is a streaming Messages request.
func AnthropicStreamBody() string {
	return `{"model":"fake-claude","max_tokens":5,"messages":[{"role":"user","content":"` + Prompt("x") + `"}],"stream":true}`
}

// CountBody is a token count request.
func CountBody() string {
	return `{"model":"fake-claude","messages":[{"role":"user","content":"` + Prompt("Ola") + `"}],"system":"s"}`
}

// OpenAIBody is a normal Chat Completions request.
func OpenAIBody() string {
	return `{"model":"fake-gpt","messages":[{"role":"user","content":"` + Prompt("Ola") + `"}]}`
}

// OpenAIStreamBody is a streaming Chat Completions request.
func OpenAIStreamBody() string {
	return `{"model":"fake-gpt","messages":[{"role":"user","content":"` + Prompt("x") + `"}],"stream":true}`
}

// Constructors lists every request builder by the name the inventory uses for it.
func Constructors() map[string]func() string {
	return map[string]func() string{
		"AnthropicBody":       AnthropicBody,
		"AnthropicStreamBody": AnthropicStreamBody,
		"CountBody":           CountBody,
		"OpenAIBody":          OpenAIBody,
		"OpenAIStreamBody":    OpenAIStreamBody,
	}
}

// WithField adds a member to a request body built by a constructor.
func WithField(body, member string) string {
	return strings.TrimSuffix(body, "}") + "," + member + "}"
}

// Needles are everything a run must never leave behind.
func Needles() []string {
	return []string{Marker(), UserMarker(), EmailID(), SpacedID()}
}

// escapedForms returns the spellings a JSON encoder or a careful log could produce.
func escapedForms(needle string) map[string]string {
	forms := map[string]string{"raw": needle}
	if quoted, err := json.Marshal(needle); err == nil {
		forms["json"] = strings.Trim(string(quoted), `"`)
	}
	forms["partial-unicode"] = unicodeEscaped(needle, false)
	forms["full-unicode"] = unicodeEscaped(needle, true)
	return forms
}

func unicodeEscaped(value string, everything bool) string {
	var out strings.Builder
	for _, char := range value {
		isPlain := (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')
		if isPlain && !everything {
			out.WriteRune(char)
			continue
		}
		fmt.Fprintf(&out, `\u%04x`, char)
	}
	return out.String()
}

// Finding names where a needle appeared, and never holds the needle or the line around
// it, so that a failure report cannot itself become a leak.
type Finding struct {
	Channel string
	Case    string
	Form    string
}

func (f Finding) String() string { return f.Channel + "/" + f.Case + " (" + f.Form + " form)" }

// Scanner looks for needles in the outputs of a run. Reading nothing is a failure, so a
// scanner pointed at the wrong place cannot pass by accident.
type Scanner struct {
	mu        sync.Mutex
	forms     map[string]string
	findings  []Finding
	failures  []string
	inspected map[string]int
}

// NewScanner returns a scanner for the given needles.
func NewScanner(needles ...string) *Scanner {
	scanner := &Scanner{forms: map[string]string{}, inspected: map[string]int{}}
	for i, needle := range needles {
		for form, text := range escapedForms(needle) {
			scanner.forms[fmt.Sprintf("%s:%d", form, i)] = text
		}
	}
	return scanner
}

// Scan examines one output.
func (s *Scanner) Scan(channel, name string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inspected[channel]++
	text := string(data)
	for key, needle := range s.forms {
		if strings.Contains(text, needle) {
			form, _, _ := strings.Cut(key, ":")
			s.findings = append(s.findings, Finding{Channel: channel, Case: name, Form: form})
		}
	}
}

// ScanFile examines a file. A file that cannot be read is a failure, not a pass.
func (s *Scanner) ScanFile(channel, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		s.fail(channel, filepath.Base(path))
		return
	}
	s.Scan(channel, filepath.Base(path), data)
}

// ScanDir examines every file under a directory and fails when it holds none.
func (s *Scanner) ScanDir(channel, dir string) {
	count := 0
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			count++
			s.ScanFile(channel, path)
		}
		return nil
	})
	if err != nil || count == 0 {
		s.fail(channel, "directory "+filepath.Base(dir))
	}
}

func (s *Scanner) fail(channel, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, channel+"/"+name)
}

// Findings returns the occurrences, in a stable order.
func (s *Scanner) Findings() []Finding {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.findings)
	slices.SortFunc(out, func(a, b Finding) int { return strings.Compare(a.String(), b.String()) })
	return slices.Compact(out)
}

// Inspected counts the outputs examined in a channel.
func (s *Scanner) Inspected(channel string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inspected[channel]
}

// Err reports occurrences, outputs that could not be read and required channels that
// were never examined.
func (s *Scanner) Err(requiredChannels ...string) error {
	var problems []string
	for _, finding := range s.Findings() {
		problems = append(problems, "found in "+finding.String())
	}
	s.mu.Lock()
	for _, failure := range s.failures {
		problems = append(problems, "could not read "+failure)
	}
	for _, channel := range requiredChannels {
		if s.inspected[channel] == 0 {
			problems = append(problems, "nothing inspected in "+channel)
		}
	}
	s.mu.Unlock()
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}

// Inventory is the content of tests/privacy/coverage.json.
type Inventory struct {
	Constructors     []string          `json:"constructors"`
	InstrumentedGo   []string          `json:"instrumented_go_files"`
	ParserOnlyGo     []ParserOnlyFile  `json:"parser_only_go_files"`
	InstrumentedSDK  string            `json:"instrumented_sdk_file"`
	Cases            []string          `json:"cases"`
	Channels         []Channel         `json:"channels"`
	Jobs             map[string]string `json:"jobs"`
	ConsumerContract []string          `json:"consumer_contract"`
}

// ParserOnlyFile holds literal prompts that stop at the parser, with the reason.
type ParserOnlyFile struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// Channel says whether a destination exists yet. An unavailable channel is recorded as
// such and never counts as a passing check.
type Channel struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	RequiredBy  string `json:"required_by,omitempty"`
	Description string `json:"description"`
}

// Channel statuses.
const (
	ChannelCovered     = "covered"
	ChannelUnavailable = "unavailable"
)

// ReadInventory parses the inventory, refusing what it does not recognize so that a
// typo cannot silently drop a requirement.
func ReadInventory(path string) (*Inventory, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var inventory Inventory
	if err := decoder.Decode(&inventory); err != nil {
		return nil, err
	}
	return &inventory, nil
}
