package sensitivedata

import "testing"

func TestCPFCheckDigits(t *testing.T) {
	valid := cpfFrom("529982247")
	cases := []struct {
		name string
		text string
		want int64
	}{
		{"plain", "cliente " + valid + " quer cancelar", 1},
		{"masked", "CPF " + maskCPF(valid) + ".", 1},
		{"start and end of text", valid, 1},
		{"wrong first check digit", "cpf " + valid[:9] + string('0'+(valid[9]-'0'+1)%10) + valid[10:], 0},
		{"wrong second check digit", "cpf " + breakLastDigit(valid), 0},
		{"masked with wrong digit", maskCPF(breakLastDigit(valid)), 0},
		{"repeated digits", "11111111111 e 111.111.111-11", 0},
		{"inside a longer number", "9" + valid + "9", 0},
		{"inside an identifier", "id" + valid, 0},
		{"glued to more digits by a separator", valid + ".5", 0},
		{"partial mask", valid[:3] + "." + valid[3:6] + "." + valid[6:], 0},
		{"two occurrences", valid + " e " + maskCPF(valid), 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countOf(t, CPF, tc.text); got != tc.want {
				t.Fatalf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCNPJCheckDigits(t *testing.T) {
	valid := cnpjFrom("112223330001")
	cases := []struct {
		name string
		text string
		want int64
	}{
		{"plain", "empresa " + valid, 1},
		{"masked", "CNPJ: " + maskCNPJ(valid) + ";", 1},
		{"wrong check digit", breakLastDigit(valid), 0},
		{"repeated", "00000000000000", 0},
		{"alphanumeric form is not covered", "12ABC34501DE35", 0},
		{"inside a longer number", valid + "1", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countOf(t, CNPJ, tc.text); got != tc.want {
				t.Fatalf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCardLuhn(t *testing.T) {
	visa := cardFrom("4", 16)
	amex := cardFrom("37", 15)
	cases := []struct {
		name string
		text string
		want int64
	}{
		{"contiguous", "cartão " + visa, 1},
		{"spaced groups", visa[:4] + " " + visa[4:8] + " " + visa[8:12] + " " + visa[12:], 1},
		{"hyphenated groups", visa[:4] + "-" + visa[4:8] + "-" + visa[8:12] + "-" + visa[12:], 1},
		{"amex groups", amex[:4] + " " + amex[4:10] + " " + amex[10:], 1},
		{"followed by an expiry", visa[:4] + " " + visa[4:8] + " " + visa[8:12] + " " + visa[12:] + " 12/27", 1},
		{"minimum length", cardFrom("5", 13), 1},
		{"maximum length", cardFrom("6", 19), 1},
		{"too short", cardFrom("5", 12), 0},
		{"too long", cardFrom("6", 20), 0},
		{"Luhn failure", breakLastDigit(visa), 0},
		{"repeated digits", "0000000000000000", 0},
		{"mixed separators", visa[:4] + " " + visa[4:8] + "-" + visa[8:12] + " " + visa[12:], 0},
		{"inside a longer number", "1" + visa, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countOf(t, Card, tc.text); got != tc.want {
				t.Fatalf("count = %d, want %d", got, tc.want)
			}
		})
	}
}
