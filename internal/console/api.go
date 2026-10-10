package console

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dboss/internal/authcog"
	"dboss/internal/fault"
	"dboss/internal/httpx"
	"dboss/internal/ops"
	"dboss/internal/pages"
	"dboss/internal/version"
)

// apiActor is the audit actor of every call authorized by tokens.dboss.
const apiActor = "api"

// API error codes. A refused call or one the caller can fix answers HTTP 400, a failure of the
// server itself (apiInternal) 500; clients branch on the code.
const (
	apiDisabled       = "api_disabled"
	apiUnauthorized   = "unauthorized"
	apiUnknownAction  = "unknown_action"
	apiInvalidRequest = "invalid_request"
	apiFailed         = "failed"
	apiRateLimited    = "rate_limited"
	apiInternal       = "internal"
)

const openAPIPath = "/api/openapi.json"

// getEndpoints are the GET routes on the management host, listed at the top of the guide.
var getEndpoints = []struct{ path, auth, desc string }{
	{"/api", "open", "this guide"},
	{openAPIPath, "open", "OpenAPI 3.1 of every action: import into Swagger UI, Postman, Insomnia, Bruno or Hoppscotch"},
	{"/healthz", "open", "200 ok while the daemon is up"},
	{"/readyz", "open", "200 while every autostart app serves, else 503 naming the apps that do not"},
	{"/metrics", "bearer", "Prometheus text; 404 without tokens.dboss"},
	{"/hooks/<app>/<hook>", "bearer", "one deploy hook's state: running, restarting, last error and output"},
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// apiResponse is the envelope: a success always carries data (null when the action returns
// nothing), a failure never does.
type apiResponse struct {
	OK    bool      `json:"ok"`
	Data  any       `json:"data"`
	Error *apiError `json:"error,omitempty"`
}

type apiFailure struct {
	OK    bool      `json:"ok"`
	Error *apiError `json:"error"`
}

func writeAPIError(w http.ResponseWriter, code, message string) {
	writeAPI(w, http.StatusBadRequest, apiFailure{Error: &apiError{Code: code, Message: message}})
}

// writeActionError answers an action that failed: 400 when the caller can fix it (fault.Invalid),
// else 500.
func writeActionError(w http.ResponseWriter, err error) {
	if fault.IsInvalid(err) {
		writeAPIError(w, apiFailed, err.Error())
		return
	}
	writeAPI(w, http.StatusInternalServerError, apiFailure{Error: &apiError{Code: apiInternal, Message: err.Error()}})
}

// writeAPI is writeJSON without HTML escaping: API answers go to scripts and agents, never into a
// page, and a message reads `<token>`, not `\u003ctoken\u003e`.
func writeAPI(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

// apiCall runs one ops action for a bearer holding tokens.dboss. The body is a JSON object of the
// action's params; the method comes from the path and the actor is always "api".
func (h *Handler) apiCall(w http.ResponseWriter, r *http.Request) {
	token := h.service.DbossToken()
	if token == "" {
		writeAPIError(w, apiDisabled, "the API is off: set tokens.dboss in dboss-server.yaml and send it as a bearer token")
		return
	}
	switch h.checkToken(w, r, func() bool { return tokenIn(httpx.BearerToken(r), token) }) {
	case tokenLimited:
		writeAPIError(w, apiRateLimited, tokenRetry(w))
		return
	case tokenWrong:
		writeAPIError(w, apiUnauthorized, "send tokens.dboss as Authorization: Bearer <token>")
		return
	}
	spec, ok := ops.SpecFor(r.PathValue("action"))
	if !ok {
		writeAPIError(w, apiUnknownAction, fmt.Sprintf("unknown action %q; GET /api lists every action", r.PathValue("action")))
		return
	}
	request, err := apiRequest(w, r, spec)
	if err != nil {
		writeAPIError(w, apiInvalidRequest, err.Error())
		return
	}
	data, err := h.service.Do(request)
	if err != nil {
		writeActionError(w, err)
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{OK: true, Data: data})
}

// apiRequest decodes the body against the action's params: an unknown or missing required param
// is refused by name, and a duration arrives as a Go duration string.
func apiRequest(w http.ResponseWriter, r *http.Request, spec ops.Spec) (ops.Request, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		return ops.Request{}, err
	}
	fields := map[string]json.RawMessage{}
	if body = bytes.TrimSpace(body); len(body) > 0 {
		if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
			return ops.Request{}, errors.New("the body must be one JSON object")
		}
	}
	params := map[string]ops.Param{}
	for _, param := range spec.Params {
		params[param.Name] = param
	}
	for name, value := range fields {
		param, ok := params[name]
		if !ok {
			return ops.Request{}, fmt.Errorf("%s takes no param %q; it takes: %s", spec.Name, name, paramNames(spec))
		}
		if string(value) == "null" {
			delete(fields, name)
			continue
		}
		if err := checkParamType(param, value); err != nil {
			return ops.Request{}, err
		}
		if param.Type != ops.TypeDuration {
			continue
		}
		var text string
		_ = json.Unmarshal(value, &text)
		duration, err := time.ParseDuration(text)
		if err != nil {
			return ops.Request{}, fmt.Errorf("%s: %v", name, err)
		}
		fields[name], _ = json.Marshal(duration)
	}
	for _, param := range spec.Params {
		if param.Required && blank(fields[param.Name]) {
			return ops.Request{}, fmt.Errorf("%s needs %s", spec.Name, param.Name)
		}
	}
	var request ops.Request
	encoded, _ := json.Marshal(fields)
	if err := json.Unmarshal(encoded, &request); err != nil {
		return ops.Request{}, err
	}
	request.Method, request.Actor = spec.Name, apiActor
	return request, nil
}

// checkParamType refuses a value of the wrong JSON type by the param's own name and type, so a
// caller never reads Go's decoder text.
func checkParamType(param ops.Param, value json.RawMessage) error {
	var ok bool
	switch param.Type {
	case ops.TypeString:
		var v string
		ok = json.Unmarshal(value, &v) == nil
	case ops.TypeDuration:
		var v string
		ok = json.Unmarshal(value, &v) == nil
		if !ok {
			return fmt.Errorf("%s must be a duration string such as \"30s\"", param.Name)
		}
	case ops.TypeInteger:
		var v int
		ok = json.Unmarshal(value, &v) == nil
	case ops.TypeBoolean:
		var v bool
		ok = json.Unmarshal(value, &v) == nil
	case ops.TypeStrings:
		var v []string
		ok = json.Unmarshal(value, &v) == nil
	case ops.TypeObject:
		var v map[string]string
		ok = json.Unmarshal(value, &v) == nil
	default:
		ok = json.Valid(value)
	}
	if !ok {
		return fmt.Errorf("%s must be %s", param.Name, typeNoun(param.Type))
	}
	return nil
}

func typeNoun(kind string) string {
	switch kind {
	case ops.TypeString:
		return "a string"
	case ops.TypeInteger:
		return "an integer"
	case ops.TypeBoolean:
		return "true or false"
	case ops.TypeStrings:
		return "an array of strings"
	case ops.TypeObject:
		return "an object of string values"
	}
	return "valid JSON"
}

// blank is a missing required value: absent, an empty or whitespace-only string, or an empty
// array of strings.
func blank(value json.RawMessage) bool {
	if len(value) == 0 {
		return true
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		return strings.TrimSpace(text) == ""
	}
	var list []string
	if json.Unmarshal(value, &list) == nil {
		return len(list) == 0
	}
	return false
}

func paramNames(spec ops.Spec) string {
	if len(spec.Params) == 0 {
		return "nothing"
	}
	names := make([]string, len(spec.Params))
	for i, param := range spec.Params {
		names[i] = param.Name
	}
	return strings.Join(names, ", ")
}

// apiNotFound answers every other /api path, so a wrong verb or path points at the help.
func (h *Handler) apiNotFound(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, apiUnknownAction, fmt.Sprintf("%s %s is not an API route: actions are POST /api/<action>, GET /api lists them", r.Method, r.URL.Path))
}

func apiBase(r *http.Request) string { return authcog.Scheme(r) + "://" + r.Host }

// apiHelp is the API guide for people and agents: how to connect and every action with its params.
// A browser gets it rendered by ui-markdown; everything else, and ?format=md, gets the markdown.
func (h *Handler) apiHelp(w http.ResponseWriter, r *http.Request) {
	guide := apiGuide(apiBase(r), h.service.DbossToken() != "")
	if !isBrowser(r) || r.URL.Query().Get("format") == "md" {
		writeText(w, guide)
		return
	}
	encoded, _ := json.Marshal(guide) // escapes <, > and &, so it cannot close the script tag
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, strings.Replace(apiPage, "{{guide}}", string(encoded), 1))
}

// isBrowser is a page load from Firefox, Chrome, Edge or Safari: Edge and Chrome carry Chrome/,
// Safari carries Safari/. A script or an agent that asks for text/html under its own name still
// gets the markdown.
func isBrowser(r *http.Request) bool {
	agent := r.Header.Get("User-Agent")
	return strings.Contains(r.Header.Get("Accept"), "text/html") &&
		(strings.Contains(agent, "Firefox/") || strings.Contains(agent, "Chrome/") || strings.Contains(agent, "Safari/"))
}

// apiPageAssets are the console assets the guide page loads. They are served without a session,
// since the guide itself is open; the rest of the console's assets stay behind sign-in.
var apiPageAssets = []string{"app.css", "fez.min.js", "marked.umd.js", "fez/ui-markdown.fez"}

const apiPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light">
  <title>dboss API</title>
  <link rel="icon" type="image/svg+xml" href="` + pages.LogoPath + `">
  <link rel="stylesheet" href="/assets/app.css">
  <script src="/assets/fez.min.js"></script>
  <script src="/assets/marked.umd.js"></script>
  <script fez="/assets/fez/ui-markdown.fez"></script>
</head>
<body>
  <main class="api-guide">
    <p class="api-guide-raw"><a href="/api?format=md">Raw markdown</a></p>
    <script type="application/json" id="api-guide">{{guide}}</script>
    <ui-markdown src="api-guide"></ui-markdown>
  </main>
</body>
</html>
`

func apiGuide(base string, enabled bool) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("# dboss API")
	line("")
	line("dboss %s at %s.", version.String(), base)
	line("Every action the dboss CLI and the console run is one `POST /api/<action>`.")
	if !enabled {
		line("")
		line("The API is off on this host: set tokens.dboss in dboss-server.yaml (or dboss-server.local.yaml) and run `dboss rescan`.")
	}
	line("")
	line("## GET endpoints")
	line("")
	writeGetEndpoints(&b)
	line("")
	line("bearer: tokens.dboss as `Authorization: Bearer <token>`.")
	line("")
	line("## Connect")
	line("")
	line("* Base URL: %s/api", base)
	line("* Auth: `Authorization: Bearer <token>`, where the token is tokens.dboss from the host config. /hooks and /metrics take the lower webhook token (`dboss token` prints it) and accept tokens.dboss too.")
	line("* Wrong tokens are throttled per client address: each one makes the next check from that address wait 3s, stacking up to 30s, after which calls are refused at once with `%s` and a Retry-After header. A client that sends the right token never waits.", apiRateLimited)
	line("* Call: `POST /api/<action>` with a JSON object of the action's params as the body. No params means an empty body or `{}`. An unknown param is refused.")
	line("* Every action is POST, reads included. The GET endpoints are the table above.")
	line("* Success: HTTP 200 `{\"ok\": true, \"data\": ...}`. `data` is null when the action returns nothing.")
	line("* Failure: `{\"ok\": false, \"error\": {\"code\": \"...\", \"message\": \"...\"}}`, HTTP 400 when the call was refused or the caller can fix it, HTTP 500 when dboss itself failed. The codes:")
	line("  * `%s` - tokens.dboss is not set", apiDisabled)
	line("  * `%s` - missing or wrong bearer token", apiUnauthorized)
	line("  * `%s` - no such action", apiUnknownAction)
	line("  * `%s` - bad body: not an object, an unknown param, a missing required one or a wrong type", apiInvalidRequest)
	line("  * `%s` - too many wrong tokens from this address; retry after Retry-After seconds", apiRateLimited)
	line("  * `%s` - the action refused the request: an unknown app or name, a state or a missing confirmation; the message says why", apiFailed)
	line("  * `%s` (HTTP 500) - the action failed on the server: a database, git or disk error; retrying may help", apiInternal)
	line("* Audit: an action marked audited writes an audit row with the actor `%s` (read them with `audit`).", apiActor)
	line("* Types: `duration` is a Go duration string (`30s`, `5m`); `json` is any JSON value; `object` is a string map.")
	line("")
	line("## Example")
	line("")
	line("```sh")
	line("curl -s -X POST %s/api/ls -H \"Authorization: Bearer $DBOSS_TOKEN\"", base)
	line("curl -s -X POST %s/api/restart -H \"Authorization: Bearer $DBOSS_TOKEN\" -d '{\"app\": \"shop\"}'", base)
	line("```")
	line("")
	line("## Notes for agents")
	line("")
	line("* Call `ls` first: it returns every app name, its state and processes.")
	line("* Read-only actions change nothing and are safe to repeat. Ask the operator before `destroy`, `pg-drop`, `pg-restore` with replace, `pg-delete-dump`, `exec`, `pg-query` and `events-query`: they delete data or run arbitrary code.")
	line("* `restart` on a running web app rolls with no downtime; `stop` drains in-flight requests first.")
	line("* A failed action is not retried by dboss; read error.message before trying again.")
	line("")
	line("## Actions")
	group := ""
	for _, spec := range ops.Specs() {
		if spec.Group != group {
			group = spec.Group
			line("")
			line("### %s", group)
		}
		line("")
		tag := ""
		if spec.Audited {
			tag = " (audited)"
		}
		line("#### POST /api/%s%s", spec.Name, tag)
		line("")
		line("%s", spec.Summary)
		if len(spec.Params) == 0 {
			continue
		}
		line("")
		line("| param | type | required | description |")
		line("| --- | --- | --- | --- |")
		for _, param := range spec.Params {
			required := "no"
			if param.Required {
				required = "yes"
			}
			line("| %s | %s | %s | %s |", param.Name, param.Type, required, strings.ReplaceAll(param.Desc, "|", "\\|"))
		}
	}
	return b.String()
}

// writeGetEndpoints renders getEndpoints as a markdown table with padded columns, so the raw
// text reads as a table in a browser too. Paths are relative to the base URL the guide opens with.
func writeGetEndpoints(b *strings.Builder) {
	rows := [][3]string{{"GET", "auth", "what"}}
	for _, endpoint := range getEndpoints {
		link := "[" + endpoint.path + "](" + endpoint.path + ")"
		if strings.Contains(endpoint.path, "<") {
			link = "`" + endpoint.path + "`" // a placeholder is no link, and raw it would render as a tag
		}
		rows = append(rows, [3]string{link, endpoint.auth, endpoint.desc})
	}
	var widths [3]int
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], len(cell))
		}
	}
	for i, row := range rows {
		fmt.Fprintf(b, "| %-*s | %-*s | %-*s |\n", widths[0], row[0], widths[1], row[1], widths[2], row[2])
		if i == 0 {
			fmt.Fprintf(b, "| %s | %s | %s |\n", strings.Repeat("-", widths[0]), strings.Repeat("-", widths[1]), strings.Repeat("-", widths[2]))
		}
	}
}

// apiOpenAPI exports every action as an OpenAPI 3.1 document.
func (h *Handler) apiOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Disposition", `inline; filename="dboss-openapi.json"`)
	writeAPI(w, http.StatusOK, openAPI(apiBase(r)))
}

type object = map[string]any

func openAPI(base string) object {
	paths := object{}
	var tags []object
	seen := map[string]bool{}
	for _, spec := range ops.Specs() {
		if !seen[spec.Group] {
			seen[spec.Group] = true
			tags = append(tags, object{"name": spec.Group})
		}
		description := spec.Summary
		if spec.Audited {
			description += "\n\nWrites an audit row with the actor `" + apiActor + "`."
		}
		operation := object{
			"operationId": operationID(spec.Name),
			"tags":        []string{spec.Group},
			"summary":     spec.Summary,
			"description": description,
			"responses": object{
				"200": object{"description": "the action ran", "content": object{"application/json": object{"schema": object{"$ref": "#/components/schemas/Success"}}}},
				"400": object{"description": "the call was refused or the caller can fix it", "content": object{"application/json": object{"schema": object{"$ref": "#/components/schemas/Failure"}}}},
				"500": object{"description": "the action failed on the server", "content": object{"application/json": object{"schema": object{"$ref": "#/components/schemas/Failure"}}}},
			},
		}
		if len(spec.Params) > 0 {
			schema, required := paramSchema(spec)
			operation["requestBody"] = object{"required": required, "content": object{"application/json": object{"schema": schema}}}
		}
		paths["/api/"+spec.Name] = object{"post": operation}
	}
	return object{
		"openapi": "3.1.0",
		"info": object{
			"title":       "dboss API",
			"version":     version.String(),
			"description": "Every action the dboss CLI and the console run. Send tokens.dboss from the host config as a bearer token. The guide is GET /api.",
		},
		"servers":  []object{{"url": base}},
		"security": []object{{"bearer": []string{}}},
		"tags":     tags,
		"paths":    paths,
		"components": object{
			"securitySchemes": object{"bearer": object{"type": "http", "scheme": "bearer", "description": "tokens.dboss from the host config"}},
			"schemas": object{
				"Success": object{
					"type":       "object",
					"required":   []string{"ok", "data"},
					"properties": object{"ok": object{"const": true}, "data": object{"description": "the action's result; null when it returns nothing"}},
				},
				"Failure": object{
					"type":     "object",
					"required": []string{"ok", "error"},
					"properties": object{
						"ok": object{"const": false},
						"error": object{
							"type":     "object",
							"required": []string{"code", "message"},
							"properties": object{
								"code":    object{"type": "string", "enum": []string{apiDisabled, apiUnauthorized, apiUnknownAction, apiInvalidRequest, apiRateLimited, apiFailed, apiInternal}},
								"message": object{"type": "string"},
							},
						},
					},
				},
			},
		},
	}
}

// paramSchema is the request body schema of one action and whether any param is required.
func paramSchema(spec ops.Spec) (object, bool) {
	properties := object{}
	required := []string{}
	for _, param := range spec.Params {
		schema := object{"description": param.Desc}
		switch param.Type {
		case ops.TypeString:
			schema["type"] = "string"
		case ops.TypeInteger:
			schema["type"] = "integer"
		case ops.TypeBoolean:
			schema["type"] = "boolean"
		case ops.TypeStrings:
			schema["type"], schema["items"] = "array", object{"type": "string"}
		case ops.TypeObject:
			schema["type"], schema["additionalProperties"] = "object", object{"type": "string"}
		case ops.TypeDuration:
			schema["type"], schema["examples"] = "string", []string{"30s"}
		}
		properties[param.Name] = schema
		if param.Required {
			required = append(required, param.Name)
		}
	}
	schema := object{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema, len(required) > 0
}

// operationID turns an action name into the identifier client generators expect: pg-backup ->
// pgBackup.
func operationID(name string) string {
	parts := strings.Split(name, "-")
	for i := 1; i < len(parts); i++ {
		parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
	}
	return strings.Join(parts, "")
}
