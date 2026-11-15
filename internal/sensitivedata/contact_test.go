package sensitivedata

import (
	"strings"
	"testing"
)

func TestEmailBoundaries(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int64
	}{
		{"plain", "Responda para cliente@example.test", 1},
		{"sentence end", "escreva para maria.souza@empresa.com.br.", 1},
		{"plus and percent", "a+b%c@mail.example.org", 1},
		{"inside query string keeps only the address", "url?to=ana@example.org&x=1", 1},
		{"two addresses", "a@example.org, b@example.org", 2},
		{"no domain dot", "user@localhost", 0},
		{"numeric tld", "user@example.123", 0},
		{"leading dot", ".user@example.org", 0},
		{"double dot", "us..er@example.org", 0},
		{"label with hyphen at the edge", "user@-example.org", 0},
		{"local part too long", strings.Repeat("a", 65) + "@example.org", 0},
		{"lonely at sign", "@ @@ a@ @b.com", 0},
		{"mention", "@joao disse oi", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countOf(t, Email, tc.text); got != tc.want {
				t.Fatalf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestBrazilianPhoneForms(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int64
	}{
		{"mobile masked", "ligue (11) 98765-4321", 1},
		{"mobile with country code", "+55 11 98765-4321", 1},
		{"country code and parentheses", "+55 (21) 99876-5432", 1},
		{"landline", "(31) 3456-7890", 1},
		{"contiguous mobile", "11987654321", 1},
		{"contiguous with country code", "5511987654321", 1},
		{"space split", "11 98765 4321", 1},
		{"unknown area code", "(20) 98765-4321", 0},
		{"mobile without leading nine", "(11) 88765-4321", 0},
		{"landline starting with nine", "(11) 9876-5432", 0},
		{"foreign country code", "+1 415 555 0100", 0},
		{"inside longer number", "119876543210", 0},
		{"short", "(11) 9876-543", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countOf(t, Phone, tc.text); got != tc.want {
				t.Fatalf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPixRandomKeyCandidates(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int64
	}{
		{"version 4 lower case", "chave 123e4567-e89b-42d3-a456-426614174000", 1},
		{"version 4 upper case", "123E4567-E89B-42D3-B456-426614174000", 1},
		{"version 1 UUID", "123e4567-e89b-12d3-a456-426614174000", 0},
		{"wrong variant", "123e4567-e89b-42d3-c456-426614174000", 0},
		{"without hyphens", "123e4567e89b42d3a456426614174000", 0},
		{"glued to a letter", "x123e4567-e89b-42d3-a456-426614174000", 0},
		{"followed by a hyphen", "123e4567-e89b-42d3-a456-426614174000-1", 0},
		{"ambiguous: any v4 UUID is a candidate", "trace_id=9f1c2b3a-4d5e-4f60-8a7b-1c2d3e4f5a6b", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countOf(t, Pix, tc.text); got != tc.want {
				t.Fatalf("count = %d, want %d", got, tc.want)
			}
		})
	}
}
