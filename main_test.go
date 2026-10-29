package main

import (
	"net/http"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cf := func(kv ...string) http.Header {
		h := http.Header{"Cf-Ray": {"a52259ecfb280c2a-AMS"}}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	env := func(r string) string {
		return `{"result":true,"csrf":"625454d2ea8f94119fae","data":{"result":"` + r + `"}}`
	}

	cases := []struct {
		name   string
		status int
		h      http.Header
		body   string
		want   verdict
	}{
		{"available", 200, cf(), env("AVAILABLE"), vAvailable},
		{"taken", 200, cf(), env("TAKEN"), vTaken},
		{"you", 200, cf(), env("YOU"), vTaken},
		{"illegal", 200, cf(), env("ILLEGAL"), vReserved},
		{"invalid", 200, cf(), env("INVALID"), vInvalid},
		{"too short", 200, cf(), env("TOO_SHORT"), vInvalid},
		{"too long", 200, cf(), env("TOO_LONG"), vInvalid},
		{"unknown result", 200, cf(), env("MAYBE"), vOdd},
		{"result false", 200, cf(), `{"result":false,"data":{"result":"TAKEN"}}`, vOdd},
		{"no cf-ray, perfect json", 200, http.Header{}, env("AVAILABLE"), vRetry},
		{"html 200", 200, cf(), "<html>ad page</html>", vRetry},
		{"truncated json", 200, cf(), `{"result":true,"data":{"res`, vRetry},
		{"407", 407, http.Header{}, "", vRetry},
		{"proxy 502", 502, http.Header{}, "", vRetry},
		{"challenge header", 403, cf("Cf-Mitigated", "challenge"), "", vBlocked},
		{"challenge body", 403, cf(), "<title>Just a moment...</title>", vBlocked},
		{"waf 1020", 403, cf(), "error code: 1020", vBlocked},
		{"bare 403", 403, cf(), "", vBlocked},
		{"429", 429, cf("Retry-After", "60"), "", vLimited},
		{"1015 as 403", 403, cf(), "error code: 1015", vLimited},
		{"origin 500", 500, cf(), "", vServer},
		{"cf 522", 522, cf(), "", vServer},
		{"cf 525", 525, cf(), "", vServer},
		{"redirect", 302, cf("Location", "/sign-in/"), "", vOdd},
		{"404", 404, cf(), "", vOdd},
	}
	for _, c := range cases {
		if got := classify(c.status, c.h, []byte(c.body)).v; got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, verdictName[got], verdictName[c.want])
		}
	}
}

func TestRetryAfter(t *testing.T) {
	if d := classify(429, http.Header{"Cf-Ray": {"x"}, "Retry-After": {"90"}}, nil).retryAfter; d != 90*time.Second {
		t.Errorf("retry-after seconds: got %s", d)
	}
	if d := retryAfter("soon"); d != 0 {
		t.Errorf("garbage retry-after: got %s", d)
	}
}

func TestGenerate(t *testing.T) {
	all := generate(charsetDefault, 2)
	if len(all) != 37*37 {
		t.Fatalf("got %d names, want %d", len(all), 37*37)
	}
	if all[0] != "aa" || all[len(all)-1] != "__" {
		t.Errorf("order: first %q last %q", all[0], all[len(all)-1])
	}
	if n := len(generate("aA-b", 2)); n != 4 {
		t.Errorf("charset cleanup: got %d names, want 4", n)
	}
}

func TestValid(t *testing.T) {
	for s, want := range map[string]bool{"ab": true, "x_": true, "a": false, "a-b": false, "abcdefghijklmnop": false, "abcdefghijklmno": true} {
		if valid(s) != want {
			t.Errorf("valid(%q) = %v", s, !want)
		}
	}
}
