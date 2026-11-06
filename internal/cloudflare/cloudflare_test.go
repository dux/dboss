package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAPI answers like Cloudflare for one zone, shop.com, and records every request.
func fakeAPI(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *[]string {
	t.Helper()
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`))
			return
		}
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		handle(w, r)
	}))
	t.Cleanup(server.Close)
	previous := APIBase
	APIBase = server.URL
	t.Cleanup(func() { APIBase = previous })
	return &calls
}

func answer(w http.ResponseWriter, result any) {
	data, _ := json.Marshal(map[string]any{"success": true, "errors": []any{}, "result": result})
	_, _ = w.Write(data)
}

// The zone is found by walking up from the hostname, so a deep name and a wildcard both land in
// shop.com, and the walk stops before the bare TLD.
func TestZoneForWalksUpToTheZone(t *testing.T) {
	calls := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") == "shop.com" {
			answer(w, []Zone{{ID: "z1", Name: "shop.com"}})
			return
		}
		answer(w, []Zone{})
	})
	client := Client{Token: "tok"}
	zone, err := client.ZoneFor(context.Background(), "*.api.shop.com")
	if err != nil || zone.ID != "z1" {
		t.Fatalf("zone = %+v, %v", zone, err)
	}
	if got := strings.Join(*calls, ","); got != "GET /zones?name=api.shop.com,GET /zones?name=shop.com" {
		t.Fatalf("calls = %s", got)
	}
	*calls = nil
	if _, err := client.ZoneFor(context.Background(), "other.org"); err == nil || !strings.Contains(err.Error(), "other.org") {
		t.Fatalf("an unknown zone answered %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("the walk asked for the TLD: %v", *calls)
	}
}

func TestRecordsCreateAndUpdate(t *testing.T) {
	var bodies []Record
	calls := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			answer(w, []Record{{ID: "r1", Type: "A", Name: "shop.com", Content: "198.51.100.1", Proxied: true}})
			return
		}
		var body Record
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("body %v, content type %q", err, r.Header.Get("Content-Type"))
		}
		bodies = append(bodies, body)
		body.ID = "r2"
		answer(w, body)
	})
	client := Client{Token: "tok"}
	records, err := client.Records(context.Background(), "z1", "shop.com")
	if err != nil || len(records) != 1 || records[0].Content != "198.51.100.1" {
		t.Fatalf("records = %+v, %v", records, err)
	}
	want := Record{Type: "A", Name: "www.shop.com", Content: "203.0.113.7", Proxied: true, TTL: AutoTTL}
	if _, err := client.Create(context.Background(), "z1", want); err != nil {
		t.Fatal(err)
	}
	want.ID = "r1"
	if _, err := client.Update(context.Background(), "z1", want); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*calls, ","); got != "GET /zones/z1/dns_records?name.exact=shop.com&per_page=100,POST /zones/z1/dns_records?,PATCH /zones/z1/dns_records/r1?" {
		t.Fatalf("calls = %s", got)
	}
	if len(bodies) != 2 || bodies[0].ID != "" || bodies[1].Content != "203.0.113.7" || !bodies[1].Proxied {
		t.Fatalf("bodies = %+v", bodies)
	}
}

// Cloudflare's own message reaches the operator, so a token without DNS:Edit says so.
func TestAPIErrorCarriesTheMessage(t *testing.T) {
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {})
	_, err := Client{Token: "wrong"}.Records(context.Background(), "z1", "shop.com")
	if err == nil || !strings.Contains(err.Error(), "Invalid access token (9109)") {
		t.Fatalf("error = %v", err)
	}
	if _, err := (Client{}).Records(context.Background(), "z1", "shop.com"); err == nil || !strings.Contains(err.Error(), TokenEnv) {
		t.Fatalf("a missing token answered %v", err)
	}
}

func TestPlan(t *testing.T) {
	want := Record{Type: "A", Name: "shop.com", Content: "203.0.113.7", Proxied: true}
	a := func(id, content string, proxied bool) Record {
		return Record{ID: id, Type: "A", Name: "shop.com", Content: content, Proxied: proxied}
	}
	cname := Record{ID: "c1", Type: "CNAME", Name: "shop.com", Content: "shop.pages.dev"}
	mx := Record{ID: "m1", Type: "MX", Name: "shop.com", Content: "mail.shop.com"}
	six := Record{ID: "s1", Type: "AAAA", Name: "shop.com", Content: "2001:db8::1"}
	for _, tc := range []struct {
		name     string
		existing []Record
		force    bool
		action   Action
		id       string
	}{
		{"empty name", nil, false, ActionCreate, ""},
		{"other types share the name", []Record{mx, six}, false, ActionCreate, ""},
		{"same record", []Record{a("r1", "203.0.113.7", true), mx}, false, ActionOK, "r1"},
		{"other address", []Record{a("r1", "198.51.100.1", true)}, false, ActionUpdate, "r1"},
		{"proxy mode differs", []Record{a("r1", "203.0.113.7", false)}, false, ActionUpdate, "r1"},
		{"two A records", []Record{a("r1", "203.0.113.7", true), a("r2", "198.51.100.1", true)}, true, ActionConflict, ""},
		{"cname without force", []Record{cname}, false, ActionConflict, ""},
		{"cname with force", []Record{cname}, true, ActionUpdate, "c1"},
	} {
		change := Plan(want, tc.existing, tc.force)
		if change.Action != tc.action || change.Want.ID != tc.id {
			t.Errorf("%s: %s on %q, want %s on %q", tc.name, change.Action, change.Want.ID, tc.action, tc.id)
		}
		if change.Action == ActionConflict && (change.Reason == "" || change.Current == nil) {
			t.Errorf("%s: a conflict without its reason or record", tc.name)
		}
	}
}
