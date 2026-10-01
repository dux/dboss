package config

import "testing"

func TestNormalizeHost(t *testing.T) {
	for raw, want := range map[string]string{
		"Example.COM.":        "example.com",
		"example.com:8080":    "example.com",
		"[::1]:443":           "::1",
		"::1":                 "::1",
		"localhost":           "localhost",
		"api.example.com.:80": "api.example.com",
	} {
		if got := NormalizeHost(raw); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestBestMatchPrefersTheMostSpecificPattern(t *testing.T) {
	patterns := []string{".Example.com", "API.example.com."}
	exact, ok := BestMatch("api.example.com", patterns)
	if !ok {
		t.Fatal("api.example.com did not match")
	}
	wildcard, ok := BestMatch("www.example.com", patterns)
	if !ok {
		t.Fatal("www.example.com did not match the leading-dot pattern")
	}
	if exact <= wildcard {
		t.Fatalf("exact score %d should beat wildcard score %d", exact, wildcard)
	}
	if _, ok := BestMatch("example.org", patterns); ok {
		t.Fatal("example.org matched")
	}
	if _, ok := BestMatch("example.com", []string{"*.example.com"}); ok {
		t.Fatal("*. pattern matched the apex")
	}
}

func TestConcreteHostAlwaysBeatsWildcard(t *testing.T) {
	patterns := []string{"*.foo.com", "baz.foo.com"}
	for _, host := range []string{"baz.foo.com", "qux.foo.com"} {
		score, ok := BestMatch(host, patterns)
		if !ok {
			t.Fatalf("%s matched nothing", host)
		}
		concrete, concreteOK := MatchHost(host, "baz.foo.com")
		wildcard, wildcardOK := MatchHost(host, "*.foo.com")
		switch host {
		case "baz.foo.com":
			if !concreteOK || concrete != score || concrete <= wildcard {
				t.Fatalf("baz.foo.com: score %d, concrete %d (%v), wildcard %d (%v)", score, concrete, concreteOK, wildcard, wildcardOK)
			}
		case "qux.foo.com":
			if concreteOK || score != wildcard {
				t.Fatalf("qux.foo.com: score %d, wildcard %d", score, wildcard)
			}
		}
	}
}

func TestWebURL(t *testing.T) {
	for _, test := range []struct {
		name      string
		scheme    string
		port      string
		canonical string
		hosts     []string
		want      string
	}{
		{"port joined", "http", "3100", "", []string{"bun.lvh.me"}, "http://bun.lvh.me:3100"},
		{"no port", "https", "", "shop.example.com", []string{"shop.example.com"}, "https://shop.example.com"},
		{"canonical wins", "https", "", "b.example.com", []string{"a.example.com", "b.example.com"}, "https://b.example.com"},
		{"leading dot matches its apex", "http", "3110", "", []string{".sinatra.lvh.me"}, "http://sinatra.lvh.me:3110"},
		{"wildcard only has no address", "http", "3100", "", []string{"*.example.com"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := WebURL(test.scheme, test.port, test.canonical, test.hosts); got != test.want {
				t.Fatalf("WebURL = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPrimaryHostPicksOneAddress(t *testing.T) {
	for _, test := range []struct {
		name      string
		canonical string
		hosts     []string
		want      string
	}{
		{"canonical wins", "b.example.com", []string{"a.example.com", "b.example.com"}, "b.example.com"},
		{"first concrete host", "", []string{"a.example.com", "b.example.com"}, "a.example.com"},
		{"leading dot matches its apex", "", []string{".sinatra.lvh.me"}, "sinatra.lvh.me"},
		{"concrete beats a pattern", "", []string{"*.example.com", "app.example.com"}, "app.example.com"},
		{"subdomains only has no apex", "", []string{"*.example.com"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := PrimaryHost(test.canonical, test.hosts); got != test.want {
				t.Fatalf("PrimaryHost = %q, want %q", got, test.want)
			}
		})
	}
}
