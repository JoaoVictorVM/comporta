package useridentity

import (
	"errors"
	"strings"
	"testing"
)

func FuzzOpaqueIdentityValidation(f *testing.F) {
	for _, seed := range []string{
		"", "a", "user_123", "joao@mail.com", "52998224725", "529.982.247-25", "11222333000181",
		"12.ABC.345/01DE-35", "12abc34501de35", "\x00", "é", "@", "a@", strings.Repeat("a", 129),
		"00000000000", "##.###.###/####-##", "ß@ı", "ıııııııııııııı",
	} {
		f.Add(seed)
	}
	service := newService(testKey())
	f.Fuzz(func(t *testing.T, id string) {
		err := ValidateID(id)
		hash, lookupErr := service.LookupHash("bot", id)
		if !errors.Is(lookupErr, err) && lookupErr != err {
			t.Fatalf("lookup and validation disagree: %v, %v", lookupErr, err)
		}
		if err == nil {
			if len(id) == 0 || len(id) > maxIDLength || len(hash.Hex()) != HashHexLength {
				t.Fatalf("accepted %q, hash %q", id, hash.Hex())
			}
			for i := range len(id) {
				if !allowedByte(id[i]) {
					t.Fatalf("accepted a byte outside the allowlist: %q", id)
				}
			}
		}
	})
}

func FuzzIdentityErrorsNeverEchoInput(f *testing.F) {
	for _, seed := range []string{"secret-value@mail.com", "secret value", "52998224725", "12ABC34501DE35"} {
		f.Add(seed)
	}
	known := []error{nil, ErrUserInvalid, ErrUserEmail, ErrUserDocument}
	f.Fuzz(func(t *testing.T, id string) {
		err := ValidateID(id)
		for _, candidate := range known {
			if err == candidate {
				return
			}
		}
		t.Fatalf("an error outside the fixed set could carry the input: %v", err)
	})
}
