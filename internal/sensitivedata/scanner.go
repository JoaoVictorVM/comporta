package sensitivedata

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

// Reason classifies a failed scan for logs and tests. It never carries content.
type Reason string

const (
	ReasonTimeout   Reason = "timeout"
	ReasonCanceled  Reason = "canceled"
	ReasonCapacity  Reason = "capacity"
	ReasonPanic     Reason = "panic"
	ReasonEntropy   Reason = "entropy"
	ReasonCollision Reason = "collision"
	ReasonInput     Reason = "input"
)

// ScanError is the only error a scan returns. Its message is fixed: no value, offset,
// recovered panic or parser error ever reaches it.
type ScanError struct {
	Reason Reason
}

func (e *ScanError) Error() string { return "sensitive data scan failed" }

// Is lets callers test for ErrScanFailed or ErrCanceled.
func (e *ScanError) Is(target error) bool {
	if target == ErrCanceled {
		return e.Reason == ReasonCanceled
	}
	return target == ErrScanFailed
}

var (
	// ErrScanFailed matches every scan failure, including a cancellation.
	ErrScanFailed = errors.New("sensitive data scan failed")
	// ErrCanceled matches the failure caused by the caller's context being canceled.
	ErrCanceled = errors.New("sensitive data scan canceled")

	errTimeout   = &ScanError{ReasonTimeout}
	errCanceled  = &ScanError{ReasonCanceled}
	errCapacity  = &ScanError{ReasonCapacity}
	errPanic     = &ScanError{ReasonPanic}
	errEntropy   = &ScanError{ReasonEntropy}
	errCollision = &ScanError{ReasonCollision}
	errInput     = &ScanError{ReasonInput}
)

// Limits bound one scan. They are not configurable by YAML; tests inject smaller ones.
type Limits struct {
	// Deadline covers selection, validation and detection together.
	Deadline time.Duration
	// MaxCandidates bounds the valid matches of one request.
	MaxCandidates int
	// MaxMapBytes bounds the unique values kept to build placeholders.
	MaxMapBytes int
	// MaxSecretBytes bounds one secret candidate.
	MaxSecretBytes int
}

// DefaultLimits are the limits of the product contract.
func DefaultLimits() Limits {
	return Limits{Deadline: 50 * time.Millisecond, MaxCandidates: 4096, MaxMapBytes: 1 << 20, MaxSecretBytes: 64 << 10}
}

// checkInterval is how many bytes a detector reads between deadline checks.
const checkInterval = 4 << 10

// Scanner runs the detectors. It holds no request state and is safe for concurrent
// use: every scan builds its own run.
type Scanner struct {
	limits  Limits
	now     func() time.Time
	entropy io.Reader
}

// Option configures a Scanner.
type Option func(*Scanner)

// WithLimits replaces the default limits.
func WithLimits(limits Limits) Option { return func(s *Scanner) { s.limits = limits } }

// WithClock replaces the clock used for the deadline.
func WithClock(now func() time.Time) Option { return func(s *Scanner) { s.now = now } }

// WithEntropy replaces the source of placeholder nonces.
func WithEntropy(r io.Reader) Option { return func(s *Scanner) { s.entropy = r } }

// NewScanner returns a Scanner with the default limits.
func NewScanner(options ...Option) *Scanner {
	s := &Scanner{limits: DefaultLimits(), now: time.Now, entropy: rand.Reader}
	for _, option := range options {
		option(s)
	}
	return s
}

// Block names what refused a request: the category of highest precedence among the
// blocked ones and, for a secret, its subtype.
type Block struct {
	Category Category
	Kind     string
}

// Message is the public refusal, built from the allowlists only.
func (b Block) Message() string {
	if b.Category == Secret {
		return fmt.Sprintf("request contains a secret (%s); blocked by policy", b.Kind)
	}
	return fmt.Sprintf("request contains sensitive data (%s); blocked by policy", b.Category)
}

// Outcome is the decision of a complete scan. Rewritten fields hold request text, so
// every generic representation shows the summary only.
type Outcome struct {
	Summary   Summary
	Blocked   *Block
	rewritten map[int]string
	reverse   *ReverseMap
}

// Rewritten returns the replacement text of a field that had values redacted.
func (o *Outcome) Rewritten(field int) (string, bool) {
	if o == nil {
		return "", false
	}
	text, ok := o.rewritten[field]
	return text, ok
}

// Redacted reports whether any field changed.
func (o *Outcome) Redacted() bool { return o != nil && len(o.rewritten) > 0 }

// Map returns the reverse map, present only when rehydration was asked for and
// something was redacted.
func (o *Outcome) Map() *ReverseMap {
	if o == nil {
		return nil
	}
	return o.reverse
}

// DetachMap hands the reverse map over to the caller, who becomes responsible for
// clearing it; Release no longer touches it.
func (o *Outcome) DetachMap() {
	if o != nil {
		o.reverse = nil
	}
}

// Release drops the rewritten fields and the reverse map. It is safe to call twice.
func (o *Outcome) Release() {
	if o == nil {
		return
	}
	clear(o.rewritten)
	o.rewritten = nil
	o.reverse.Clear()
	o.reverse = nil
}

func (o *Outcome) String() string {
	if o == nil {
		return "sensitivedata.Outcome{}"
	}
	return fmt.Sprintf("sensitivedata.Outcome{detections:%d blocked:%t redacted:%t}", len(o.Summary), o.Blocked != nil, o.Redacted())
}

// Format writes the safe string for every verb.
func (o *Outcome) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, o.String()) }

// LogValue writes the safe string.
func (o *Outcome) LogValue() slog.Value { return slog.StringValue(o.String()) }

// MarshalJSON writes the summary only.
func (o *Outcome) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Emit hands one string of the request to the scan, in document order.
type Emit func(text string) error

// Scan reads the strings a caller emits and decides. With no active category it
// returns a nil outcome without calling visit, so off never reads content. A panic in
// visit or in a detector becomes a sanitized failure. The result of a scan that ends
// after the deadline is discarded even if every check along the way passed.
func (s *Scanner) Scan(ctx context.Context, policy Policy, visit func(Emit) error) (outcome *Outcome, err error) {
	if !policy.Active() {
		return nil, nil
	}
	r := &run{ctx: ctx, limits: s.limits, now: s.now, policy: policy, deadline: s.now().Add(s.limits.Deadline)}
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(r.deadline) {
		r.deadline = deadline
	}
	defer r.release()
	defer func() {
		if recover() != nil {
			outcome.Release()
			outcome, err = nil, errPanic
		}
	}()
	if err := r.check(); err != nil {
		return nil, err
	}
	if err := visit(r.add); err != nil {
		var scanErr *ScanError
		if errors.As(err, &scanErr) {
			return nil, scanErr
		}
		return nil, errInput
	}
	outcome, err = r.decide(s.entropy)
	if err == nil {
		err = r.check()
	}
	if err != nil {
		outcome.Release()
		return nil, err
	}
	return outcome, nil
}

type match struct {
	field, start, end int
	category          Category
	kind              string
}

type matchKey struct {
	field, start, end int
	category          Category
}

// run is the state of one scan. It owns copies of the request strings until the
// scan returns.
type run struct {
	ctx      context.Context
	limits   Limits
	now      func() time.Time
	policy   Policy
	deadline time.Time
	fields   []string
	matches  []match
	seen     map[matchKey]struct{}
	nextTick int
}

func (r *run) release() {
	clear(r.fields)
	r.fields = nil
	r.matches = nil
	r.seen = nil
}

func (r *run) check() error {
	if err := r.ctx.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			return errCanceled
		}
		return errTimeout
	}
	if r.now().After(r.deadline) {
		return errTimeout
	}
	return nil
}

func (r *run) resetTick() { r.nextTick = checkInterval }

// tick checks the deadline whenever a detector crosses another block of bytes.
func (r *run) tick(position int) error {
	if position < r.nextTick {
		return nil
	}
	r.nextTick = position + checkInterval
	return r.check()
}

func (r *run) emit(start, end int, c Category, kind string) error {
	key := matchKey{len(r.fields) - 1, start, end, c}
	if _, duplicate := r.seen[key]; duplicate {
		return nil
	}
	if len(r.matches) >= r.limits.MaxCandidates {
		return errCapacity
	}
	if r.seen == nil {
		r.seen = make(map[matchKey]struct{})
	}
	r.seen[key] = struct{}{}
	r.matches = append(r.matches, match{key.field, start, end, c, kind})
	return nil
}

func (r *run) active(c Category) bool { return r.policy.Mode(c) != ModeOff }

// add scans one string. Strings are never joined, so a value split between two
// fields is not a match.
func (r *run) add(text string) error {
	if err := r.check(); err != nil {
		return err
	}
	r.fields = append(r.fields, text)
	if r.active(CPF) || r.active(CNPJ) || r.active(Card) || r.active(Phone) {
		if err := r.numbers(text); err != nil {
			return err
		}
	}
	if r.active(Email) {
		if err := r.emails(text); err != nil {
			return err
		}
	}
	if r.active(Pix) {
		if err := r.uuids(text); err != nil {
			return err
		}
	}
	if r.active(Secret) {
		return (&secretScan{r: r, s: text}).all()
	}
	return nil
}

func (r *run) numbers(s string) error {
	r.resetTick()
	wantCPF, wantCNPJ, wantCard, wantPhone := r.active(CPF), r.active(CNPJ), r.active(Card), r.active(Phone)
	// A phone read with its country code also contains a shorter phone; only the
	// longest reading starting first is the occurrence.
	phoneEnd := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isDigit(c) && c != '+' && c != '(' {
			continue
		}
		if err := r.tick(i); err != nil {
			return err
		}
		if !numberStart(s, i) {
			continue
		}
		if isDigit(c) {
			if wantCPF || wantCNPJ {
				if err := documentAt(s, i, wantCPF, wantCNPJ, r.emit); err != nil {
					return err
				}
			}
			if wantCard {
				if err := cardAt(s, i, r.emit); err != nil {
					return err
				}
			}
		}
		if wantPhone && i >= phoneEnd {
			if end, ok := phoneAt(s, i); ok {
				phoneEnd = end
				if err := r.emit(i, end, Phone, "brazilian_phone"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *run) emails(s string) error {
	r.resetTick()
	for from := 0; from < len(s); {
		k := strings.IndexByte(s[from:], '@')
		if k < 0 {
			return nil
		}
		at := from + k
		from = at + 1
		if err := r.tick(at); err != nil {
			return err
		}
		if start, end, ok := emailAt(s, at); ok {
			if err := r.emit(start, end, Email, "email"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *run) uuids(s string) error {
	r.resetTick()
	for from := 0; from < len(s); {
		k := strings.IndexByte(s[from:], '-')
		if k < 0 {
			return nil
		}
		dash := from + k
		from = dash + 1
		if err := r.tick(dash); err != nil {
			return err
		}
		start := dash - 8
		if uuidAt(s, start) {
			if err := r.emit(start, start+uuidLength, Pix, "pix_random_key"); err != nil {
				return err
			}
			from = start + uuidLength
		}
	}
	return nil
}

// decide turns the matches into the summary, a refusal or the replacements.
func (r *run) decide(entropy io.Reader) (*Outcome, error) {
	outcome := &Outcome{Summary: r.summary()}
	if block := r.block(); block != nil {
		outcome.Blocked = block
		return outcome, nil
	}
	rewritten, reverse, err := r.redact(entropy)
	if err != nil {
		return nil, err
	}
	outcome.rewritten, outcome.reverse = rewritten, reverse
	return outcome, nil
}

func (r *run) summary() Summary {
	var counts [categoryCount]int64
	kinds := map[string]bool{}
	for _, m := range r.matches {
		counts[m.category]++
		if m.category == Secret {
			kinds[m.kind] = true
		}
	}
	var summary Summary
	for _, c := range Categories() {
		if counts[c] == 0 {
			continue
		}
		d := Detection{DataType: c, Count: counts[c], Action: r.policy.Mode(c)}
		if c == Secret {
			for _, kind := range secretKinds {
				if kinds[kind] {
					d.SecretKinds = append(d.SecretKinds, kind)
				}
			}
		}
		summary = append(summary, d)
	}
	return summary
}

// block picks the category of highest precedence among the blocked matches and, for
// a secret, the kind of its first occurrence in the request.
func (r *run) block() *Block {
	var chosen *match
	for i := range r.matches {
		m := &r.matches[i]
		if r.policy.Mode(m.category) != ModeBlock {
			continue
		}
		if chosen == nil || precedence[m.category] < precedence[chosen.category] ||
			(m.category == chosen.category && (m.field < chosen.field || (m.field == chosen.field && m.start < chosen.start))) {
			chosen = m
		}
	}
	if chosen == nil {
		return nil
	}
	block := &Block{Category: chosen.category}
	if chosen.category == Secret {
		block.Kind = chosen.kind
	}
	return block
}
