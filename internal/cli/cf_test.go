package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"dboss/internal/cloudflare"
	"dboss/internal/config"
)

func TestDNSNames(t *testing.T) {
	app, err := config.ParseApp([]byte(`domain: shop.com
procfile:
  web:
    command: ./server
    subdomain: ["@", api]
  admin:
    command: ./admin
    host: [admin.example.org, ".tenant.example.org", "*.edge.example.org", "mail.*", 10.0.0.5, api.shop.com]
`), filepath.Join(t.TempDir(), config.FileName), config.Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	names, skipped := dnsNames(app)
	want := []string{"admin.example.org", "tenant.example.org", "*.tenant.example.org", "*.edge.example.org", "api.shop.com", "shop.com", "www.shop.com"}
	if !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	if !slices.Equal(skipped, []string{"mail.*", "10.0.0.5"}) {
		t.Fatalf("skipped = %v", skipped)
	}
}

// One zone with shop.com already right, www missing and api on a CNAME: the plan shows all
// three, --set creates www only and reports the CNAME it left alone.
func TestCfDNSCreatesMissingRecordsAndLeavesConflicts(t *testing.T) {
	var writes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := func(result any) {
			data, _ := json.Marshal(map[string]any{"success": true, "result": result})
			_, _ = w.Write(data)
		}
		switch {
		case r.URL.Path == "/zones":
			if r.URL.Query().Get("name") == "shop.com" {
				reply([]cloudflare.Zone{{ID: "z1", Name: "shop.com"}})
				return
			}
			reply([]cloudflare.Zone{})
		case r.Method == http.MethodGet:
			switch r.URL.Query().Get("name.exact") {
			case "shop.com":
				reply([]cloudflare.Record{{ID: "r1", Type: "A", Name: "shop.com", Content: "203.0.113.7", Proxied: true}})
			case "api.shop.com":
				reply([]cloudflare.Record{{ID: "c1", Type: "CNAME", Name: "api.shop.com", Content: "elsewhere.net"}})
			default:
				reply([]cloudflare.Record{})
			}
		default:
			var body cloudflare.Record
			_ = json.NewDecoder(r.Body).Decode(&body)
			writes = append(writes, r.Method+" "+body.Type+" "+body.Name+" "+body.Content)
			reply(body)
		}
	}))
	t.Cleanup(server.Close)
	previous := cloudflare.APIBase
	cloudflare.APIBase = server.URL
	t.Cleanup(func() { cloudflare.APIBase = previous })

	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{config.FileName: "domain: shop.com\nprocfile:\n  web:\n    command: ./server\n    subdomain: [\"@\", api]\n"})
	t.Setenv(cloudflare.TokenEnv, "tok")
	args := []string{"-c", filepath.Join(dir, config.FileName), "--ip", "203.0.113.7"}

	out := &bytes.Buffer{}
	if err := (CLI{Out: out, Err: &bytes.Buffer{}}).cfDNS(args); err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{"shop.com ", "www.shop.com ", "api.shop.com ", "CNAME elsewhere.net", "1 change to apply; run with --set"} {
		if !strings.Contains(out.String(), row) {
			t.Fatalf("plan lacks %q:\n%s", row, out)
		}
	}
	if len(writes) != 0 {
		t.Fatalf("the plan wrote %v", writes)
	}

	out.Reset()
	err := (CLI{Out: out, Err: &bytes.Buffer{}}).cfDNS(append(args, "--set"))
	if err == nil || !strings.Contains(err.Error(), "1 name not set") {
		t.Fatalf("--set with a conflict answered %v", err)
	}
	if !slices.Equal(writes, []string{"POST A www.shop.com 203.0.113.7"}) {
		t.Fatalf("writes = %v", writes)
	}
	if !strings.Contains(out.String(), "created A www.shop.com -> 203.0.113.7") {
		t.Fatalf("output:\n%s", out)
	}
}
