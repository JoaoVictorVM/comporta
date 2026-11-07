package proxy

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
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

func TestIdentityAndUsageRootInsertionsCompose(t *testing.T) {
	upstream, server := startUpstream(t, sseReply("data: [DONE]\n\n"))
	fx := newFixture(t, server.URL, fixtureOptions{})
	body := `{"model":"fake-gpt","messages":[],"stream":true}`
	if response, text := fx.post("/v1/chat/completions", "openai", body); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, text)
	}
	hash := fx.identity.AnonymousHash("bot").Hex()
	want := `{"model":"fake-gpt","messages":[],"stream":true,"stream_options":{"include_usage":true},"user":"` + hash + `"}`
	if got := upstream.lastBody(); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if !json.Valid([]byte(upstream.lastBody())) {
		t.Fatal("invalid JSON")
	}

	present := `{"model":"fake-gpt","messages":[],"user":"user_123","stream":true}`
	if response, text := fx.post("/v1/chat/completions", "openai", present); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, text)
	}
	replaced := `{"model":"fake-gpt","messages":[],"user":"` + hashOf(t, fx, "user_123") + `","stream":true,"stream_options":{"include_usage":true}}`
	if got := upstream.lastBody(); got != replaced {
		t.Fatalf("got  %s\nwant %s", got, replaced)
	}

	last := `{"model":"fake-gpt","messages":[],"stream":true,"user":"user_123"}`
	if response, text := fx.post("/v1/chat/completions", "openai", last); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, text)
	}
	adjacent := `{"model":"fake-gpt","messages":[],"stream":true,"user":"` + hashOf(t, fx, "user_123") + `","stream_options":{"include_usage":true}}`
	if got := upstream.lastBody(); got != adjacent {
		t.Fatalf("a replacement next to an insertion: %s", got)
	}
}

func TestMetadataInsertionPreservesUnknownBytes(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})
	body := "{ \"model\" : \"fake-claude\",\n\t\"max_tokens\":1,\"messages\":[],\"metadata\": {\"a\\u00e9\":1.50 , \"z\":[ null ]\n},\"n\":123456789012345678901234567890 }  \n"
	if response, text := fx.post("/v1/messages", "anthropic", body); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, text)
	}
	hash := fx.identity.AnonymousHash("bot").Hex()
	want := strings.Replace(body, `[ null ]`, `[ null ],"user_id":"`+hash+`"`, 1)
	if got := upstream.lastBody(); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestIdentityPatchNotExcludedWithUsagePatch(t *testing.T) {
	service := newTestIdentityService(t)
	apply := func(id string) *prepared {
		result := mustParse(t, RouteChatCompletions, `{"model":"m","messages":[],"stream":true}`)
		identity, err := service.Resolve("bot", id, true, false)
		if err != nil {
			t.Fatal(err)
		}
		patches, failure := identityPatches(result.document, RouteChatCompletions, identity.Hash().Hex())
		if failure != nil {
			t.Fatalf("failure %+v", failure)
		}
		result.patches = append(result.patches, patches...)
		result.refreshHashes()
		return result
	}
	first, second := apply("user_1"), apply("user_2")
	// Both patches insert at the same offset: telling them apart by position would drop
	// the identity from the hash along with the usage request.
	if first.patches[0].At != first.patches[1].At {
		t.Fatalf("the case needs insertions at one offset: %+v", first.patches)
	}
	if first.envelope.RequestHash == second.envelope.RequestHash {
		t.Fatal("two users must not share a request hash")
	}
	hash, err := service.LookupHash("bot", "user_1")
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(`{"model":"m","messages":[],"stream":true,"user":"` + hash.Hex() + `"}`))
	if first.envelope.RequestHash != want {
		t.Fatal("the hash must cover the identity and leave out the usage request")
	}
}
