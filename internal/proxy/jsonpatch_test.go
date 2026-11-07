package proxy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestUnknownFieldsByteIdentical(t *testing.T) {
	body := []byte("{ \"model\" : \"m\",\n\t\"future\":{\"nested\":[1.0,2e3,\"\\u00e9\\/\"],\"b\":null},\"n\":123456789012345678901234567890 }  \n")
	document, err := ParseDocument(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := document.Apply(nil); &got[0] != &body[0] {
		t.Fatal("an untouched body must be the original slice")
	}
}

func TestPatchChangesOnlyTargetSpan(t *testing.T) {
	body := `{"a": 1.50 , "model":  "old" ,"z":"é"}`
	document, err := ParseDocument([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	patch, err := document.SetMember(document.Root(), "model", []byte(`"new"`))
	if err != nil {
		t.Fatal(err)
	}
	got := string(document.Apply([]Patch{patch}))
	want := `{"a": 1.50 , "model":  "new" ,"z":"é"}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestRejectDuplicateKeysAndExtraDocument(t *testing.T) {
	var syntax *syntaxError
	cases := map[string]struct {
		body string
		want error
	}{
		"top-level duplicate": {`{"a":1,"a":2}`, errDuplicate},
		"nested duplicate":    {`{"unknown":{"x":[{"k":1,"k":2}]}}`, errDuplicate},
		"escaped duplicate":   {`{"model":"a","model":"b"}`, errDuplicate},
		"root array":          {`[1]`, errNotObject},
		"root string":         {`"x"`, errNotObject},
		"bad utf8":            {"{\"a\":\"\xff\"}", errBadUTF8},
		"too deep":            {strings.Repeat(`{"a":`, maxJSONDepth+1) + "1" + strings.Repeat("}", maxJSONDepth+1), errTooDeep},
		"too deep array":      {`{"a":` + strings.Repeat("[", maxJSONDepth+1) + strings.Repeat("]", maxJSONDepth+1) + "}", errTooDeep},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDocument([]byte(tc.body)); !errors.Is(err, tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
	for _, body := range []string{`{"a":1}{"b":2}`, `{"a":1} x`, `{"a":}`, ``, `{`} {
		if _, err := ParseDocument([]byte(body)); !errors.As(err, &syntax) {
			t.Errorf("%q: got %v", body, err)
		}
	}
	if _, err := ParseDocument([]byte(`{"a":1,}`)); err == nil || !strings.Contains(err.Error(), "invalid JSON at byte 8") {
		t.Fatalf("offset missing: %v", err)
	}
	deepest := strings.Repeat(`{"a":`, maxJSONDepth-1) + "1" + strings.Repeat("}", maxJSONDepth-1)
	if _, err := ParseDocument([]byte(deepest)); err != nil {
		t.Fatalf("depth within the limit refused: %v", err)
	}
}

func TestEscapesWhitespaceAndNestedInsertion(t *testing.T) {
	cases := map[string]struct {
		body string
		path []string
		key  string
		want string
	}{
		"empty object": {`{}`, nil, "k", `{"k":true}`},
		"empty with spaces": {
			`{  }`, nil, "k", `{"k":true  }`,
		},
		"after the last member": {
			"{\"a\":1 \n}", nil, "k", "{\"a\":1,\"k\":true \n}",
		},
		"nested object": {
			`{"o":{"x":"\"}"},"p":2}`, []string{"o"}, "k", `{"o":{"x":"\"}","k":true},"p":2}`,
		},
		"escaped key is replaced": {
			`{"k":false,"z":1}`, nil, "k", `{"k":true,"z":1}`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			document, err := ParseDocument([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			object, ok := document.Path(tc.path...)
			if !ok {
				t.Fatal("path missing")
			}
			patch, err := document.SetMember(object, tc.key, []byte("true"))
			if err != nil {
				t.Fatal(err)
			}
			got := document.Apply([]Patch{patch})
			if string(got) != tc.want {
				t.Fatalf("got %s", got)
			}
			if !json.Valid(got) {
				t.Fatalf("invalid result: %s", got)
			}
		})
	}
}

func TestSimultaneousPatches(t *testing.T) {
	document, err := ParseDocument([]byte(`{"a":1,"b":{"c":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	inner, _ := document.Path("b")
	first, _ := document.SetMember(document.Root(), "a", []byte("10"))
	second, _ := document.SetMember(inner, "d", []byte("3"))
	third, _ := document.SetMember(document.Root(), "e", []byte("4"))
	got := string(document.Apply([]Patch{third, second, first}))
	if want := `{"a":10,"b":{"c":2,"d":3},"e":4}`; got != want {
		t.Fatalf("got %s", got)
	}
}

func TestMembersAndElementsLocateValues(t *testing.T) {
	document, err := ParseDocument([]byte(` {"s":"a,b}","n":-1.5e3,"t":true,"l":[ 1 , "x" , {"y":[]} ],"o":{}} `))
	if err != nil {
		t.Fatal(err)
	}
	members := document.Members(document.Root())
	want := map[string]string{"s": `"a,b}"`, "n": `-1.5e3`, "t": `true`, "l": `[ 1 , "x" , {"y":[]} ]`, "o": `{}`}
	if len(members) != len(want) {
		t.Fatalf("members: %+v", members)
	}
	for _, member := range members {
		if got := string(document.Bytes(member.Value)); got != want[member.Key] {
			t.Errorf("%s: %s", member.Key, got)
		}
	}
	list, _ := document.Path("l")
	if elements := document.Elements(list); len(elements) != 3 || string(document.Bytes(elements[1])) != `"x"` {
		t.Fatalf("elements: %+v", elements)
	}
}
