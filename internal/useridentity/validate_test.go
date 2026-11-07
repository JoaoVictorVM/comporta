package useridentity

import (
	"errors"
	"strings"
	"testing"
)

func checkValidation(t *testing.T, cases map[string]error) {
	t.Helper()
	for id, want := range cases {
		if got := ValidateID(id); !errors.Is(got, want) && got != want {
			t.Errorf("ValidateID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestOpaqueIDBoundaries(t *testing.T) {
	checkValidation(t, map[string]error{
		"":                                  ErrUserInvalid,
		"a":                                 nil,
		strings.Repeat("a", 128):            nil,
		strings.Repeat("a", 129):            ErrUserInvalid,
		"user_123":                          nil,
		"tenant:42.user-7":                  nil,
		"with space":                        ErrUserInvalid,
		" leading":                          ErrUserInvalid,
		"trailing ":                         ErrUserInvalid,
		"tab\tinside":                       ErrUserInvalid,
		"line\nbreak":                       ErrUserInvalid,
		"nul\x00byte":                       ErrUserInvalid,
		"joão":                              ErrUserInvalid,
		"ｆｕｌｌｗｉｄｔｈ":                         ErrUserInvalid,
		"slash/inside":                      ErrUserInvalid,
		"9f2c41ab0123456789abcdef01234567":  nil,
		"anonymous":                         nil,
		strings.Repeat("é", 64):             ErrUserInvalid,
		strings.Repeat("a", 127) + "é":      ErrUserInvalid,
		"plus+sign":                         ErrUserInvalid,
		"percent%20":                        ErrUserInvalid,
		"quote\"inside":                     ErrUserInvalid,
		"backslash\\inside":                 ErrUserInvalid,
		"control\x7f":                       ErrUserInvalid,
		"emoji\xf0\x9f\x98\x80":             ErrUserInvalid,
		"invalid\xff":                       ErrUserInvalid,
		"dots...and---dashes___and:::colon": nil,
	})
}

func TestEmailRejectedBeforeCharset(t *testing.T) {
	checkValidation(t, map[string]error{
		"joao@mail.com":                          ErrUserEmail,
		"a@b":                                    ErrUserEmail,
		"joão@mail.com":                          ErrUserEmail,
		"a@b@c":                                  ErrUserEmail,
		"@mail.com":                              ErrUserInvalid,
		"joao@":                                  ErrUserInvalid,
		"@":                                      ErrUserInvalid,
		"joao @mail.com":                         ErrUserInvalid,
		strings.Repeat("a", 125) + "@b.com":      ErrUserInvalid,
		strings.Repeat("a", 122) + "@b.com":      ErrUserEmail,
		"joao@mail com":                          ErrUserInvalid,
		"line\nbreak@mail.com":                   ErrUserInvalid,
		"tab\t@mail.com":                         ErrUserInvalid,
		"joao@mail.com ":                         ErrUserInvalid,
		"JOAO@MAIL.COM":                          ErrUserEmail,
		"user+tag@mail.com":                      ErrUserEmail,
		"first.last-name_x:y@sub.mail.example.c": ErrUserEmail,
	})
}

func TestCPFCheckDigits(t *testing.T) {
	checkValidation(t, map[string]error{
		"52998224725":      ErrUserDocument,
		"529.982.247-25":   ErrUserDocument,
		"11144477735":      ErrUserDocument,
		"111.444.777-35":   ErrUserDocument,
		"52998224724":      nil,
		"52998224735":      nil,
		"529.982.247-24":   nil,
		"529982247-25":     nil,
		"529.982.247.25":   nil,
		"x52998224725":     nil,
		"52998224725x":     nil,
		"id-52998224725":   nil,
		"529.982.24-725":   nil,
		"5299822472":       nil,
		"529982247251":     nil,
		"abc.def.ghi-jk":   nil,
		"529.982.247-2X":   nil,
		"52998224x25":      nil,
		"00000000191":      ErrUserDocument,
		"000.000.001-91":   ErrUserDocument,
		"00000000192":      nil,
		"000.000.001-92":   nil,
		"12345678909":      ErrUserDocument,
		"123.456.789-09":   ErrUserDocument,
		"12345678900":      nil,
		"123.456.789-00":   nil,
		"1234567890":       nil,
		"123456789099":     nil,
		"user_12345678909": nil,
	})
}

func TestCNPJCheckDigits(t *testing.T) {
	checkValidation(t, map[string]error{
		"11222333000181":     ErrUserDocument,
		"11.222.333/0001-81": ErrUserDocument,
		"11222333000182":     nil,
		"11222333000171":     nil,
		"11.222.333/0001-82": ErrUserInvalid,
		"11.222.333/0001-71": ErrUserInvalid,
		"11-222-333-0001-81": nil,
		"id11222333000181":   nil,
		"11222333000181x":    nil,
	})
}

func TestAlphanumericCNPJCheckDigits(t *testing.T) {
	checkValidation(t, map[string]error{
		"12.ABC.345/01DE-35": ErrUserDocument,
		"12ABC34501DE35":     ErrUserDocument,
		"12abc34501de35":     ErrUserDocument,
		"12.abc.345/01de-35": ErrUserDocument,
		"12ABC34501DE36":     nil,
		"12ABC34501DE45":     nil,
		"12.ABC.345/01DE-36": ErrUserInvalid,
		"12.ABC.345/01DE-45": ErrUserInvalid,
		"12ABC34501DE3A":     nil,
		"12ABC34501DEA5":     nil,
		"12_BC34501DE35":     nil,
		"12ABC34501DE35-":    nil,
	})
}

func TestDocumentRepeatedDigits(t *testing.T) {
	cases := map[string]error{}
	for _, digit := range "0123456789" {
		cases[strings.Repeat(string(digit), 11)] = nil
		cases[strings.Repeat(string(digit), 14)] = nil
	}
	checkValidation(t, cases)
}

func TestNoImplicitNormalization(t *testing.T) {
	service := newService(testKey())
	upper, err := service.LookupHash("bot", "USER_123")
	if err != nil {
		t.Fatal(err)
	}
	lower, err := service.LookupHash("bot", "user_123")
	if err != nil {
		t.Fatal(err)
	}
	if upper == lower {
		t.Fatal("identifiers must be case sensitive")
	}
	if _, err := service.LookupHash("bot", " user_123"); !errors.Is(err, ErrUserInvalid) {
		t.Fatalf("surrounding space must not be trimmed: %v", err)
	}
	// Lowercase letters read as a CNPJ for detection only.
	if !errors.Is(ValidateID("12abc34501de35"), ErrUserDocument) {
		t.Fatal("detection must uppercase its temporary copy")
	}
	if !isCNPJ("12abc34501de35") || isCPF("12abc34501de35") {
		t.Fatal("classification")
	}
}
