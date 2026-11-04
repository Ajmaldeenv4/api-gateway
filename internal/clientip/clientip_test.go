package clientip

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolve(t *testing.T) {
	proxies := []string{"10.0.0.0/8", "192.0.2.1"}
	cases := []struct {
		name    string
		trusted []string
		remote  string
		xff     []string
		want    string
	}{
		{name: "port stripped", remote: "1.2.3.4:5678", want: "1.2.3.4"},
		{name: "ipv6 peer", remote: "[2001:db8::1]:443", want: "2001:db8::1"},
		{name: "no proxies, header ignored", remote: "1.2.3.4:1", xff: []string{"9.9.9.9"}, want: "1.2.3.4"},
		{name: "untrusted peer, header ignored", trusted: proxies, remote: "1.2.3.4:1", xff: []string{"9.9.9.9"}, want: "1.2.3.4"},
		{name: "trusted peer", trusted: proxies, remote: "10.0.0.5:1", xff: []string{"9.9.9.9"}, want: "9.9.9.9"},
		{name: "single trusted IP", trusted: proxies, remote: "192.0.2.1:1", xff: []string{"9.9.9.9"}, want: "9.9.9.9"},
		{name: "forged left entry", trusted: proxies, remote: "10.0.0.5:1", xff: []string{"6.6.6.6, 9.9.9.9"}, want: "9.9.9.9"},
		{name: "proxy chain", trusted: proxies, remote: "10.0.0.5:1", xff: []string{"9.9.9.9, 10.0.0.7"}, want: "9.9.9.9"},
		{name: "split headers", trusted: proxies, remote: "10.0.0.5:1", xff: []string{"6.6.6.6", "9.9.9.9"}, want: "9.9.9.9"},
		{name: "only proxies", trusted: proxies, remote: "10.0.0.5:1", xff: []string{"10.0.0.7"}, want: "10.0.0.5"},
		{name: "trusted peer, no header", trusted: proxies, remote: "10.0.0.5:1", want: "10.0.0.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := New(tc.trusted)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}

			var got string
			res.Middleware()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = From(r)
			})).ServeHTTP(httptest.NewRecorder(), req)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFrom_WithoutMiddlewareUsesPeer(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	req.Header.Set("X-Forwarded-For", "9.9.9.9")
	if got := From(req); got != "1.2.3.4" {
		t.Fatalf("got %q", got)
	}
}

func TestNew_RejectsBadEntry(t *testing.T) {
	if _, err := New([]string{"not-an-ip"}); err == nil {
		t.Fatal("expected error")
	}
}
