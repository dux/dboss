// Package cloudflare is the small part of the Cloudflare API that `dboss cf` needs: find the zone
// of a hostname, read the records at a name, create and update one.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIBase is the Cloudflare API root; a variable so a test can point at a local server.
var APIBase = "https://api.cloudflare.com/client/v4"

// TokenEnv names the environment variable the API token is read from, the name Cloudflare's own
// tools use.
const TokenEnv = "CLOUDFLARE_API_TOKEN"

// AutoTTL is Cloudflare's "automatic" TTL, the only one a proxied record has.
const AutoTTL = 1

var httpClient = &http.Client{Timeout: 30 * time.Second}

// ErrNoZone is ZoneFor's answer for a hostname no zone the token reads holds.
var ErrNoZone = errors.New("no Cloudflare zone this token can read holds it")

// Client calls the API with one token, which needs Zone:Read and DNS:Edit on the zones it touches.
type Client struct {
	Token string
}

type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Record struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl,omitempty"`
}

// ZoneFor finds the zone a hostname belongs to by asking for each of its parent domains in turn,
// longest first, so a delegated subzone wins over its parent. A leading "*." is not a label.
func (c Client) ZoneFor(ctx context.Context, hostname string) (Zone, error) {
	name := strings.TrimPrefix(strings.ToLower(strings.TrimSuffix(hostname, ".")), "*.")
	for candidate := name; strings.Contains(candidate, "."); {
		var zones []Zone
		if err := c.do(ctx, http.MethodGet, "/zones", url.Values{"name": {candidate}}, nil, &zones); err != nil {
			return Zone{}, err
		}
		if len(zones) > 0 {
			return zones[0], nil
		}
		_, candidate, _ = strings.Cut(candidate, ".")
	}
	return Zone{}, fmt.Errorf("%s: %w", hostname, ErrNoZone)
}

// Records lists every record at exactly name, whatever its type.
func (c Client) Records(ctx context.Context, zoneID, name string) ([]Record, error) {
	var records []Record
	query := url.Values{"name.exact": {name}, "per_page": {"100"}}
	err := c.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(zoneID)+"/dns_records", query, nil, &records)
	return records, err
}

func (c Client) Create(ctx context.Context, zoneID string, record Record) (Record, error) {
	record.ID = ""
	var created Record
	err := c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/dns_records", nil, record, &created)
	return created, err
}

// Update rewrites the type, content and proxy mode of an existing record in place.
func (c Client) Update(ctx context.Context, zoneID string, record Record) (Record, error) {
	if record.ID == "" {
		return Record{}, errors.New("update needs the record id")
	}
	var updated Record
	err := c.do(ctx, http.MethodPatch, "/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(record.ID), nil, record, &updated)
	return updated, err
}

// envelope is the shape of every Cloudflare API answer.
type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c Client) do(ctx context.Context, method, path string, query url.Values, body, result any) error {
	if c.Token == "" {
		return fmt.Errorf("no Cloudflare API token: set $%s", TokenEnv)
	}
	target := APIBase + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	var answer envelope
	if err := json.Unmarshal(data, &answer); err != nil {
		return fmt.Errorf("cloudflare %s %s: %s", method, path, response.Status)
	}
	if !answer.Success || response.StatusCode >= 300 {
		messages := make([]string, 0, len(answer.Errors))
		for _, e := range answer.Errors {
			messages = append(messages, fmt.Sprintf("%s (%d)", e.Message, e.Code))
		}
		if len(messages) == 0 {
			messages = append(messages, response.Status)
		}
		return fmt.Errorf("cloudflare %s %s: %s", method, path, strings.Join(messages, "; "))
	}
	return json.Unmarshal(answer.Result, result)
}
