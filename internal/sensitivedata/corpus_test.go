package sensitivedata

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"testing"
	"time"
)

var reportPath = flag.String("detection-report", "", "write the corpus quality report to this path, relative to the package directory")

const (
	corpusPath    = "testdata/corpus.jsonl"
	publishedPath = "../../docs/detection-report.json"
	reportCommand = "go test ./internal/sensitivedata -run '^TestCorpusReport$' -count=1 -args -detection-report=../../docs/detection-report.json"
	minimumCases  = 100
)

type corpusCase struct {
	ID       string   `json:"id"`
	Category string   `json:"category"`
	Label    string   `json:"label"`
	Text     string   `json:"text"`
	Spans    [][2]int `json:"spans"`
	Note     string   `json:"note,omitempty"`
}

type occurrenceMetrics struct {
	Expected  int      `json:"expected"`
	Predicted int      `json:"predicted"`
	TP        int      `json:"true_positives"`
	FP        int      `json:"false_positives"`
	FN        int      `json:"false_negatives"`
	Precision *float64 `json:"precision"`
	Recall    *float64 `json:"recall"`
}

type documentMetrics struct {
	TP  int      `json:"true_positives"`
	FP  int      `json:"false_positives"`
	FN  int      `json:"false_negatives"`
	TN  int      `json:"true_negatives"`
	FPR *float64 `json:"false_positive_rate"`
}

type annotatedMetrics struct {
	Cases   int `json:"cases"`
	Alerted int `json:"alerted"`
}

type categoryReport struct {
	Category    string            `json:"category"`
	Positive    int               `json:"positive_cases"`
	Negative    int               `json:"negative_cases"`
	Occurrences occurrenceMetrics `json:"occurrences"`
	Documents   documentMetrics   `json:"documents"`
	Annotated   annotatedMetrics  `json:"annotated_negatives"`
}

type anyAlert struct {
	Documents int      `json:"negative_documents"`
	Alerted   int      `json:"alerted"`
	Rate      *float64 `json:"rate"`
}

type corpusReport struct {
	SchemaVersion      int              `json:"schema_version"`
	DetectorRevision   string           `json:"detector_revision"`
	PhoneTableRevision string           `json:"phone_table_revision"`
	Command            string           `json:"command"`
	Corpus             corpusInfo       `json:"corpus"`
	Method             []string         `json:"method"`
	Limitations        []string         `json:"limitations"`
	Categories         []categoryReport `json:"categories"`
	AnyAlert           anyAlert         `json:"negative_documents_with_any_alert"`
}

type corpusInfo struct {
	Path   string `json:"path"`
	Cases  int    `json:"cases"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

var corpusTemplate = regexp.MustCompile(`\{\{secret:([a-z_]+):(\d+)\}\}`)

const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// syntheticBytes derives stable bytes from a label: the same on every run, system
// and Go version.
func syntheticBytes(label string, n int) []byte {
	out := make([]byte, 0, n)
	for block := 0; len(out) < n; block++ {
		sum := sha256.Sum256([]byte(label + ":" + strconv.Itoa(block)))
		out = append(out, sum[:min(len(sum), n-len(out))]...)
	}
	return out
}

func syntheticChars(label string, n int, alphabet string) string {
	out := syntheticBytes(label, n)
	for i, b := range out {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
}

// syntheticSecret builds the credential shape a corpus template names. Complete
// shapes are generated here instead of stored, like every other fixture of the
// repository, so the corpus file never looks like a leak.
func syntheticSecret(kind string, n int) (string, bool) {
	label := kind + ":" + strconv.Itoa(n)
	pem := func(name string) string {
		body := base64.StdEncoding.EncodeToString(syntheticBytes(label, 96))
		return "-----BEGIN " + name + "-----\n" + body[:64] + "\n" + body[64:] + "\n-----END " + name + "-----"
	}
	segment := func(value string) string { return base64.RawURLEncoding.EncodeToString([]byte(value)) }
	switch kind {
	case "anthropic":
		return "sk-" + "ant-" + "api03-" + syntheticChars(label, 60, alnum+"-_"), true
	case "openai_project":
		return "sk-" + "proj-" + syntheticChars(label, 48, alnum+"_-"), true
	case "openai":
		return "sk-" + syntheticChars(label, 32, alnum), true
	case "aws":
		return "AK" + "IA" + syntheticChars(label, 16, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"), true
	case "github_classic":
		return "gh" + "p_" + syntheticChars(label, 36, alnum), true
	case "github_fine_grained":
		return "github" + "_pat_" + syntheticChars(label, 22, alnum) + "_" + syntheticChars(label+":tail", 40, alnum), true
	case "jwt":
		return segment(`{"alg":"HS256","typ":"JWT"}`) + "." + segment(`{"sub":"`+syntheticChars(label, 8, alnum)+`"}`) + "." + syntheticChars(label+":sig", 43, alnum+"-_"), true
	case "pem_private":
		return pem("PRIVATE KEY"), true
	case "pem_rsa":
		return pem("RSA PRIVATE KEY"), true
	case "pem_ec":
		return pem("EC PRIVATE KEY"), true
	case "pem_encrypted":
		return pem("ENCRYPTED PRIVATE KEY"), true
	case "pem_incomplete":
		return "-----BEGIN " + "PRIVATE KEY-----\n" + base64.StdEncoding.EncodeToString(syntheticBytes(label, 30)), true
	}
	return "", false
}

// expand replaces the templates of a case. In a positive case every generated value
// is an expected span.
func (c *corpusCase) expand() error {
	matches := corpusTemplate.FindAllStringSubmatchIndex(c.Text, -1)
	if len(matches) == 0 {
		return nil
	}
	if len(c.Spans) > 0 {
		return fmt.Errorf("case %s mixes templates and literal spans", c.ID)
	}
	var out []byte
	last := 0
	for _, m := range matches {
		n, _ := strconv.Atoi(c.Text[m[4]:m[5]])
		value, ok := syntheticSecret(c.Text[m[2]:m[3]], n)
		if !ok {
			return fmt.Errorf("case %s names an unknown template", c.ID)
		}
		out = append(out, c.Text[last:m[0]]...)
		start := len(out)
		out = append(out, value...)
		if c.Label == "positive" {
			c.Spans = append(c.Spans, [2]int{start, len(out)})
		}
		last = m[1]
	}
	c.Text = string(append(out, c.Text[last:]...))
	return nil
}

func ratio(numerator, denominator int) *float64 {
	if denominator == 0 {
		return nil
	}
	value := float64(numerator) / float64(denominator)
	return &value
}

func loadCorpus(t *testing.T) ([]corpusCase, []byte) {
	t.Helper()
	data, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	var cases []corpusCase
	seen := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		var c corpusCase
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&c); err != nil {
			t.Fatalf("corpus line %d: %v", len(cases)+1, err)
		}
		if err := c.expand(); err != nil {
			t.Fatal(err)
		}
		if seen[c.ID] {
			t.Fatalf("duplicate case %s", c.ID)
		}
		seen[c.ID] = true
		if _, ok := ParseCategory(c.Category); !ok || (c.Label != "positive" && c.Label != "negative") {
			t.Fatalf("case %s has an invalid label", c.ID)
		}
		if (c.Label == "positive") != (len(c.Spans) > 0) {
			t.Fatalf("case %s: positives need spans and negatives have none", c.ID)
		}
		for _, span := range c.Spans {
			if span[0] < 0 || span[1] <= span[0] || span[1] > len(c.Text) {
				t.Fatalf("case %s has a span outside its text", c.ID)
			}
		}
		cases = append(cases, c)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return cases, data
}

// spansOf runs the detectors of the policy on one text and returns the spans found
// for a category, in order. It reads the run directly: positions never leave the
// package through its public API.
func spansOf(t *testing.T, policy Policy, text string, c Category) [][2]int {
	t.Helper()
	limits := DefaultLimits()
	limits.Deadline = time.Hour
	r := &run{ctx: context.Background(), limits: limits, now: time.Now, policy: policy, deadline: time.Now().Add(time.Hour)}
	if err := r.add(text); err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	var spans [][2]int
	for _, m := range r.matches {
		if m.category == c {
			spans = append(spans, [2]int{m.start, m.end})
		}
	}
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	return spans
}

func buildReport(t *testing.T) []byte {
	t.Helper()
	cases, data := loadCorpus(t)
	digest := sha256.Sum256(data)
	report := corpusReport{
		SchemaVersion: 1, DetectorRevision: DetectorRevision, PhoneTableRevision: PhoneTableRevision, Command: reportCommand,
		Corpus: corpusInfo{Path: "internal/sensitivedata/" + corpusPath, Cases: len(cases), Bytes: len(data), SHA256: hex.EncodeToString(digest[:])},
		Method: []string{
			"Each case is labeled for one category and is scanned with only that category in detect mode.",
			"Occurrences: a predicted span counts as a true positive only when its start, end and category equal an expected span; counts are per occurrence, so a repeated value counts each time.",
			"Documents: each case and its category form one binary classification of presence; false_positive_rate is FP/(FP+TN).",
			"Overlaps are not resolved for counting: every valid candidate is counted in its own category, as in production summaries. Redaction keeps the longest span, with ties broken by secret, cpf, cnpj, card, email, phone, pix.",
			"annotated_negatives counts the negative cases that carry a note (ambiguous shapes or out of scope forms) and how many of them raised an alert.",
			"negative_documents_with_any_alert scans every negative case with all seven categories in detect mode.",
			"A null ratio means its denominator is zero.",
			"Positive secret cases are stored as templates and generated deterministically by the runner, so the repository holds no complete credential shape.",
		},
		Limitations: []string{
			"The corpus is entirely synthetic; its rates are not an estimate of production traffic.",
			"pix reports any canonical version 4 UUID as a random key candidate; nothing in the text proves a PIX registration, so UUIDs used as trace IDs are false positives.",
			"A number that passes Luhn is reported as a card even when it is an order number; an 11-digit number with a valid area code is reported as a phone.",
			"The alphanumeric CNPJ is not covered.",
			"Names, addresses and other free-text personal data are not detected; there is no NER.",
		},
	}
	everything := Policy{}
	for _, c := range Categories() {
		everything.Set(c, ModeDetect)
	}
	for _, category := range Categories() {
		entry := categoryReport{Category: category.String()}
		policy := Policy{}
		policy.Set(category, ModeDetect)
		for _, c := range cases {
			if c.Category != category.String() {
				continue
			}
			predicted := spansOf(t, policy, c.Text, category)
			entry.Occurrences.Expected += len(c.Spans)
			entry.Occurrences.Predicted += len(predicted)
			for _, span := range predicted {
				if slices.Contains(c.Spans, span) {
					entry.Occurrences.TP++
				} else {
					entry.Occurrences.FP++
				}
			}
			for _, span := range c.Spans {
				if !slices.Contains(predicted, span) {
					entry.Occurrences.FN++
				}
			}
			alerted := len(predicted) > 0
			switch {
			case c.Label == "positive" && alerted:
				entry.Positive++
				entry.Documents.TP++
			case c.Label == "positive":
				entry.Positive++
				entry.Documents.FN++
			case alerted:
				entry.Negative++
				entry.Documents.FP++
			default:
				entry.Negative++
				entry.Documents.TN++
			}
			if c.Label == "negative" && c.Note != "" {
				entry.Annotated.Cases++
				if alerted {
					entry.Annotated.Alerted++
				}
			}
		}
		o := &entry.Occurrences
		o.Precision, o.Recall = ratio(o.TP, o.TP+o.FP), ratio(o.TP, o.TP+o.FN)
		entry.Documents.FPR = ratio(entry.Documents.FP, entry.Documents.FP+entry.Documents.TN)
		if entry.Positive < minimumCases || entry.Negative < minimumCases {
			t.Fatalf("%s needs at least %d positive and %d negative cases", category, minimumCases, minimumCases)
		}
		report.Categories = append(report.Categories, entry)
	}
	for _, c := range cases {
		if c.Label != "negative" {
			continue
		}
		report.AnyAlert.Documents++
		for _, category := range Categories() {
			if len(spansOf(t, everything, c.Text, category)) > 0 {
				report.AnyAlert.Alerted++
				break
			}
		}
	}
	report.AnyAlert.Rate = ratio(report.AnyAlert.Alerted, report.AnyAlert.Documents)
	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// TestCorpusReport computes the quality report twice and requires identical bytes,
// then writes it when -detection-report names a path.
func TestCorpusReport(t *testing.T) {
	first, second := buildReport(t), buildReport(t)
	if !bytes.Equal(first, second) {
		t.Fatal("the report is not reproducible")
	}
	if *reportPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(*reportPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(*reportPath, first, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPublishedReportIsCurrent keeps the published numbers tied to the corpus and
// the rules: any change to either must regenerate the report.
func TestPublishedReportIsCurrent(t *testing.T) {
	published, err := os.ReadFile(publishedPath)
	if err != nil {
		t.Fatalf("published report missing; run: %s", reportCommand)
	}
	if !bytes.Equal(bytes.ReplaceAll(published, []byte("\r\n"), []byte("\n")), buildReport(t)) {
		t.Fatalf("docs/detection-report.json is stale; run: %s", reportCommand)
	}
}
