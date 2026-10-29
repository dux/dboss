package config

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPubsubPath is the path a bare `pubsub: true` enables on the web process.
const DefaultPubsubPath = "/socketio"

// DefaultStatic is the directory a web process serves static files from unless it sets `static`.
const DefaultStatic = "./public"

// DefaultPages is the folder, relative to the app (or the host), holding the HTML pages dboss
// serves for it: <name>.html per page, template.html for every page.
const DefaultPages = "./public/error_pages"

// DevDomain is the suffix a single-app config that declares no host or subdomain binds to: `<app>.lvh.me`,
// so `dboss start` inside an app folder serves it with no config.
const DevDomain = "lvh.me"

// ProcessSpec is one entry of a procfile: the command to run, the hosts the single web process
// answers, its optional realtime hub, its readiness check and the canonical hostname. A scalar
// value is the command alone, so a background process stays a one-liner; a mapping adds the rest.
type ProcessSpec struct {
	Command string `yaml:"command" json:"command"`
	// Host is the full host pattern, or a list of them, the process serves as written.
	Host List `yaml:"host,omitempty" json:"host,omitempty"`
	// Subdomain is one label or wildcard pattern, or a list of them, prepended to the app's
	// domain when the process sets no host; "@" is the domain itself. So an app on foo.bar whose
	// web process sets subdomain: ["@", api] serves foo.bar and api.foo.bar, and www.foo.bar
	// redirects to foo.bar.
	Subdomain List        `yaml:"subdomain,omitempty" json:"subdomain,omitempty"`
	Pubsub    *PubsubSpec `yaml:"pubsub,omitempty" json:"pubsub,omitempty"`
	// Health is the web process's readiness path, e.g. /up; empty means a TCP connect.
	Health string `yaml:"health,omitempty" json:"health,omitempty"`
	// CanonicalHost is the web process hostname every other host redirects to; it must be one
	// of Hosts.
	CanonicalHost string `yaml:"canonical_host,omitempty" json:"canonical_host,omitempty"`
	// Count is how many copies of the process run; 0 means one. Copies of a web process share
	// its hosts and the proxy balances between them.
	Count int `yaml:"count,omitempty" json:"count,omitempty"`
	// Roll false stops each copy of this web process before its replacement starts, instead of
	// starting the new copy beside the old one, for a process that must never run twice. The
	// lifecycle start step still runs first, while the old copy serves. nil means true.
	Roll *bool `yaml:"roll,omitempty" json:"roll,omitempty"`
	// BasicAuth and Password replace the app-level values for this web process only; nil
	// inherits, an empty value turns the gate off here.
	BasicAuth map[string]string `yaml:"basic_auth,omitempty" json:"-"`
	Password  *string           `yaml:"password,omitempty" json:"-"`
	// Vibe docks the AI harness shell onto every page this web process serves.
	Vibe bool `yaml:"vibe,omitempty" json:"vibe,omitempty"`
}

// MaxCount bounds procfile count, so one typo cannot claim the whole port range.
const MaxCount = 64

// Instances is how many copies of the process run.
func (p ProcessSpec) Instances() int { return max(p.Count, 1) }

// Rolls reports whether a restart may start the new copy before the old one stops.
func (p ProcessSpec) Rolls() bool { return p.Roll == nil || *p.Roll }

// processSpecKeys are the keys of the mapping form, in the order the hints name them. They are
// checked here because a custom decoder is a leaf as far as the schema walk is concerned.
var processSpecKeys = []string{"command", "host", "subdomain", "pubsub", "health", "canonical_host", "count", "roll", "basic_auth", "password", "vibe"}

// processSpecFields is ProcessSpec without its methods, so the mapping form decodes and encodes
// through the struct tags instead of recursing into the custom marshalers.
type processSpecFields ProcessSpec

// UnmarshalYAML accepts a bare command string or the processSpecKeys mapping.
func (p *ProcessSpec) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*p = ProcessSpec{Command: node.Value}
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if key := node.Content[i].Value; !slices.Contains(processSpecKeys, key) {
				if hint, ok := movedKeys["procfile."+key]; ok {
					return &Error{Line: node.Content[i].Line, Key: "procfile", Message: fmt.Sprintf("%q was removed", key), Hint: hint}
				}
				return &Error{Line: node.Content[i].Line, Key: "procfile", Message: fmt.Sprintf("unknown key %q", key), Hint: "valid keys here: " + strings.Join(processSpecKeys, ", ")}
			}
		}
		return node.Decode((*processSpecFields)(p))
	}
	return &Error{Line: node.Line, Key: "procfile", Message: "must be a command or a {" + strings.Join(processSpecKeys, ", ") + "} mapping"}
}

// commandOnly reports whether the process only runs a command, which marshals as a scalar.
func (p ProcessSpec) commandOnly() bool {
	return len(p.Host) == 0 && len(p.Subdomain) == 0 && p.Pubsub == nil && p.Health == "" && p.CanonicalHost == "" && p.Count <= 1 && p.Roll == nil && p.BasicAuth == nil && p.Password == nil && !p.Vibe
}

// MarshalYAML writes a scalar command when the process only runs a command, else the full mapping,
// so a resolved config reads like the file it came from.
func (p ProcessSpec) MarshalYAML() (any, error) {
	if p.commandOnly() {
		return p.Command, nil
	}
	return processSpecFields(p), nil
}

// MarshalJSON mirrors MarshalYAML for `dboss config -d --json`.
func (p ProcessSpec) MarshalJSON() ([]byte, error) {
	if p.commandOnly() {
		return json.Marshal(p.Command)
	}
	return json.Marshal(processSpecFields(p))
}

// StaticPath is the app-level static file option: a directory served straight from disk, true for
// the default ./public, or false to disable. An absent option uses the default.
type StaticPath string

// UnmarshalYAML accepts true (the default directory), a path, or false (disabled).
func (s *StaticPath) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return &Error{Line: node.Line, Key: "static", Message: "must be true, a path, or false"}
	}
	switch node.Tag {
	case "!!bool":
		if node.Value == "true" {
			*s = DefaultStatic
		}
		return nil
	case "!!null":
		return nil
	}
	*s = StaticPath(node.Value)
	return nil
}

// MarshalYAML writes false when static serving is off, else the directory.
func (s StaticPath) MarshalYAML() (any, error) {
	if s == "" {
		return false, nil
	}
	return string(s), nil
}

func (s StaticPath) MarshalJSON() ([]byte, error) {
	if s == "" {
		return []byte("false"), nil
	}
	return json.Marshal(string(s))
}

// UnmarshalJSON mirrors UnmarshalYAML for the console's JSON config payloads.
func (s *StaticPath) UnmarshalJSON(data []byte) error {
	var path string
	if err := json.Unmarshal(data, &path); err == nil {
		*s = StaticPath(path)
		return nil
	}
	var enabled bool
	if err := json.Unmarshal(data, &enabled); err != nil {
		return err
	}
	if enabled {
		*s = DefaultStatic
		return nil
	}
	*s = ""
	return nil
}

// PubsubSpec is the web process's realtime option: `true` for the default path, a bare path, or a
// mapping of the full pubsub settings. An empty or absent option disables the hub, and an empty
// path disables it too, matching the path-as-switch rule.
type PubsubSpec struct {
	Path     string
	Settings *PubsubOverrides
}

func (p PubsubSpec) enabled() bool { return p.Path != "" }

// UnmarshalYAML accepts true, a path string, or a {path, secret, replay, ...} mapping.
func (p *PubsubSpec) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!bool":
			if node.Value == "true" {
				p.Path = DefaultPubsubPath
			}
			return nil
		case "!!null":
			return nil
		}
		p.Path = node.Value
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if key := node.Content[i].Value; !validPubsubKey(key) {
				return &Error{Line: node.Content[i].Line, Key: "pubsub", Message: fmt.Sprintf("unknown key %q", key), Hint: "valid keys here: path, secret, replay, max_clients, max_message_size, client_events, test"}
			}
		}
		var settings PubsubOverrides
		if err := node.Decode(&settings); err != nil {
			return err
		}
		p.Settings = &settings
		if settings.Path != nil {
			p.Path = *settings.Path
		}
		return nil
	}
	return &Error{Line: node.Line, Key: "pubsub", Message: "must be true, a path, or a mapping of pubsub settings"}
}

// MarshalYAML writes the mapping form when the full block was given, else the path shorthand.
func (p PubsubSpec) MarshalYAML() (any, error) {
	if p.Settings != nil {
		return p.Settings, nil
	}
	return p.Path, nil
}

func (p PubsubSpec) MarshalJSON() ([]byte, error) {
	if p.Settings != nil {
		return json.Marshal(p.Settings)
	}
	return json.Marshal(p.Path)
}

func validPubsubKey(key string) bool {
	switch key {
	case "path", "secret", "replay", "max_clients", "max_message_size", "client_events", "test":
		return true
	}
	return false
}

// VibePath is where a web process with vibe serves the harness, on each of its own hosts.
const VibePath = "/_dboss_/vibe"

// Vibe is a web process's resolved AI harness. It has no gate of its own: whoever passes the web
// process's password, basic_auth or auth gets the shell with the app.
type Vibe struct {
	Enabled bool `json:"enabled"`
}

// WebProcess is one procfile entry that answers proxied traffic: the hosts it serves, the
// canonical hostname its other hosts redirect to, and its realtime hub. An app may have several,
// each bound to its own PORT.
type WebProcess struct {
	Name  string
	Hosts List
	// Subdomain marks hosts derived from subdomain + domain; they yield to a host claimed with
	// `host`, in this app or another one.
	Subdomain     bool
	CanonicalHost string
	// Redirect is the www or apex sibling a subdomain process answers with a 301; its From is
	// the last of Hosts. It yields to every other claim on that hostname.
	Redirect *HostRedirect
	Pubsub   Pubsub
	// Static is the directory served straight from disk; empty disables it.
	Static string
	// BasicAuth and Password are the app-level gates with the process's own values applied.
	BasicAuth map[string]string
	Password  string
	Vibe      Vibe
}

type App struct {
	Procfile map[string]ProcessSpec `yaml:"procfile" json:"procfile"`
	// Domain is the app's own domain, e.g. foo.bar. A web process that sets a subdomain serves
	// it with each subdomain prepended; a process with a host ignores it.
	Domain string `yaml:"domain" json:"domain,omitempty"`
	// Branch is the git branch the app's checkout must be on: a start switches to it, a pull
	// deploy refuses any other. Empty leaves the checkout alone.
	Branch string `yaml:"branch" json:"branch,omitempty"`
	// WebProcesses and Hosts are derived from every procfile entry that declares a host or a subdomain; they
	// are never written back to YAML and exist for the supervisor and proxy.
	WebProcesses []WebProcess       `yaml:"-" json:"-"`
	Hosts        List               `yaml:"-" json:"-"`
	Autostart    Autostart          `yaml:"autostart" json:"autostart"`
	Deletable    bool               `yaml:"deletable" json:"deletable"`
	Cron         map[string]CronJob `yaml:"cron" json:"cron"`
	Hooks        map[string]Hook    `yaml:"hooks" json:"hooks"`
	// Lifecycle maps a step (create, start, destroy) to the command dboss runs at that point.
	Lifecycle map[string]LifecycleCommand `yaml:"lifecycle" json:"lifecycle,omitempty"`
	// Pages is the folder of the app's own dboss pages, relative to the app.
	Pages     string `yaml:"pages" json:"pages"`
	Defaults  `yaml:",inline"`
	Processes map[string]ProcessOverrides `yaml:"processes" json:"processes"`
}

// processNames lists procfile names sorted, so every derived choice and error is deterministic.
func processNames(procfile map[string]ProcessSpec) []string {
	names := make([]string, 0, len(procfile))
	for name := range procfile {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsWeb reports whether a procfile entry serves proxied traffic because it declares a host or a subdomain.
func (a App) IsWeb(name string) bool {
	for _, web := range a.WebProcesses {
		if web.Name == name {
			return true
		}
	}
	return false
}

// deriveWeb builds one WebProcess per procfile entry that declares a host or a subdomain; every
// other entry is a background process. The final host list is resolved later, once host defaults
// and the app overrides have been applied.
func (a *App) deriveWeb() error {
	for _, name := range processNames(a.Procfile) {
		spec := a.Procfile[name]
		if !jobName.MatchString(name) {
			return keyErr("procfile", "invalid process name %q", name)
		}
		if strings.TrimSpace(spec.Command) == "" {
			return keyErr("procfile."+name, "command is empty")
		}
		if spec.Count < 0 || spec.Count > MaxCount {
			return keyErr("procfile."+name+".count", "must be between 1 and %d", MaxCount)
		}
		for _, host := range spec.Host {
			if !validHostPattern(host) {
				return keyErr("procfile."+name+".host", "invalid host pattern %q", host)
			}
		}
		for _, subdomain := range spec.Subdomain {
			if strings.TrimSpace(subdomain) == "" {
				return &Error{Key: "procfile." + name + ".subdomain", Message: "subdomain is empty", Hint: `use "@" for the bare domain`}
			}
			if !validSubdomain(subdomain) {
				return keyErr("procfile."+name+".subdomain", "invalid subdomain %q", subdomain)
			}
		}
		if len(spec.Host) > 0 && len(spec.Subdomain) > 0 {
			return &Error{Key: "procfile." + name, Message: "host and subdomain cannot both be set", Hint: "host is the full hostname; subdomain is prepended to the app's domain"}
		}
		if len(spec.Host) == 0 && len(spec.Subdomain) == 0 {
			if spec.Roll != nil {
				return &Error{Key: "procfile." + name + ".roll", Message: "roll is only valid on a web process", Hint: "background processes are always stopped before they start again"}
			}
			continue
		}
		a.WebProcesses = append(a.WebProcesses, WebProcess{Name: name, Hosts: spec.Host, Subdomain: len(spec.Subdomain) > 0, CanonicalHost: spec.CanonicalHost})
	}
	if a.Domain != "" && !slices.ContainsFunc(a.WebProcesses, func(web WebProcess) bool { return web.Subdomain }) {
		return &Error{Key: "domain", Message: "is set but no procfile entry has a subdomain", Hint: `add subdomain: "@" to the web process (the bare domain; www redirects to it)`}
	}
	return nil
}

// resolveHosts builds each web process's final host list: its host as written, or each subdomain
// prepended to the app's domain (lvh.me in a dev session with no domain). A pattern a host
// process claims is dropped from the subdomain processes; two host processes, or two subdomain
// processes, sharing one is an error. The www/apex redirect is added last, only while no other
// process covers its hostname.
func (a *App) resolveHosts(dev bool) error {
	for index := range a.WebProcesses {
		web := &a.WebProcesses[index]
		if web.Subdomain {
			domain := a.Domain
			if domain == "" && dev {
				domain = DevDomain
			}
			if domain == "" {
				return &Error{Key: "procfile." + web.Name + ".subdomain", Message: "needs a domain on the app", Hint: "set domain at the top level of the app file, or use host for a full hostname"}
			}
			web.Hosts, web.Redirect = subdomainHosts(domain, a.Procfile[web.Name].Subdomain)
		}
		web.Hosts = uniquePatterns(web.Hosts)
		for _, host := range web.Hosts {
			if !validHostPattern(host) {
				return keyErr("procfile."+web.Name+".subdomain", "produces invalid host pattern %q", host)
			}
		}
	}
	explicit := map[string]string{}
	for _, web := range a.WebProcesses {
		if web.Subdomain {
			continue
		}
		for _, host := range web.Hosts {
			normalized := NormalizePattern(host)
			if owner, ok := explicit[normalized]; ok {
				return &Error{Key: "procfile." + web.Name, Message: fmt.Sprintf("host pattern %q is already used by process %q", host, owner)}
			}
			explicit[normalized] = web.Name
		}
	}
	derived := map[string]string{}
	for index := range a.WebProcesses {
		web := &a.WebProcesses[index]
		if !web.Subdomain {
			continue
		}
		kept := List{}
		taker := ""
		for _, host := range web.Hosts {
			normalized := NormalizePattern(host)
			if owner, ok := explicit[normalized]; ok {
				taker = owner
				continue
			}
			if owner, ok := derived[normalized]; ok {
				return &Error{Key: "procfile." + web.Name, Message: fmt.Sprintf("host pattern %q is already used by process %q", host, owner)}
			}
			derived[normalized] = web.Name
			kept = append(kept, host)
		}
		if len(kept) == 0 {
			return &Error{Key: "procfile." + web.Name + ".subdomain", Message: fmt.Sprintf("every host it produces is served by process %q through host", taker)}
		}
		web.Hosts = kept
	}
	for index := range a.WebProcesses {
		web := &a.WebProcesses[index]
		if web.Redirect == nil {
			continue
		}
		var others []string
		for _, other := range a.WebProcesses {
			if other.Name != web.Name {
				others = append(others, other.Hosts...)
			}
		}
		if !slices.ContainsFunc(web.Hosts, func(host string) bool { return NormalizePattern(host) == web.Redirect.To }) || hostAllowed(web.Redirect.From, others) {
			web.Redirect = nil
			continue
		}
		web.Hosts = append(web.Hosts, web.Redirect.From)
	}
	a.collectHosts()
	return nil
}

// Claims lists the host patterns a web process claims: its hosts without the redirect hostname,
// which never contends with another claim.
func (w WebProcess) Claims() []string {
	if w.Redirect == nil {
		return w.Hosts
	}
	return slices.DeleteFunc(slices.Clone(w.Hosts), func(host string) bool { return NormalizePattern(host) == w.Redirect.From })
}

// DropRedirects removes every www/apex redirect whose hostname covered reports as claimed
// elsewhere. It works on a copy of the web processes, like Yield.
func (a *App) DropRedirects(covered func(host string) bool) {
	webs := slices.Clone(a.WebProcesses)
	for index := range webs {
		web := &webs[index]
		if web.Redirect != nil && covered(web.Redirect.From) {
			web.Hosts = web.Claims()
			web.Redirect = nil
		}
	}
	a.WebProcesses = webs
	a.collectHosts()
}

// Yield drops a subdomain-derived host pattern that another app claims with host, and the
// www/apex redirect to it. It fails when a process would be left with no host or lose its
// canonical_host, so the app is refused instead of quietly serving less than its file says.
func (a *App) Yield(pattern, owner string) error {
	pattern = NormalizePattern(pattern)
	webs := slices.Clone(a.WebProcesses)
	for index := range webs {
		web := &webs[index]
		if !web.Subdomain {
			continue
		}
		kept := List{}
		for _, host := range web.Claims() {
			if NormalizePattern(host) != pattern {
				kept = append(kept, host)
			}
		}
		if len(kept) == len(web.Claims()) {
			continue
		}
		if len(kept) == 0 {
			return fmt.Errorf("procfile.%s: every host of its subdomain is served by app %s through host", web.Name, owner)
		}
		if web.Redirect != nil && web.Redirect.To == pattern {
			web.Redirect = nil
		}
		if web.Redirect != nil {
			kept = append(kept, web.Redirect.From)
		}
		if web.CanonicalHost != "" && !hostAllowed(web.CanonicalHost, kept) {
			return fmt.Errorf("procfile.%s.canonical_host: %s is served by app %s through host", web.Name, web.CanonicalHost, owner)
		}
		web.Hosts = kept
	}
	a.WebProcesses = webs
	a.collectHosts()
	return nil
}

// collectHosts unions every web process's hosts onto App.Hosts, in process order.
func (a *App) collectHosts() {
	a.Hosts = nil
	for _, web := range a.WebProcesses {
		a.Hosts = append(a.Hosts, web.Hosts...)
	}
}

// uniquePatterns drops repeated patterns, keeping the first spelling, in a new slice so the
// procfile's own list is never rewritten.
func uniquePatterns(hosts List) List {
	seen := map[string]bool{}
	out := make(List, 0, len(hosts))
	for _, host := range hosts {
		if normalized := NormalizePattern(host); !seen[normalized] {
			seen[normalized] = true
			out = append(out, host)
		}
	}
	return out
}

// resolveStatic copies the app-level static directory onto every web process, so the option lives
// in one place. An empty value disables serving.
func (a *App) resolveStatic() {
	for index := range a.WebProcesses {
		a.WebProcesses[index].Static = string(a.Web.Static)
	}
}

// resolveAccess gives every web process the app-level basic_auth and password, replaced by the
// procfile entry's own value when it sets one, so two web processes of one app can differ.
func (a *App) resolveAccess() error {
	for index := range a.WebProcesses {
		web := &a.WebProcesses[index]
		spec := a.Procfile[web.Name]
		web.BasicAuth, web.Password = a.Web.BasicAuth, a.Web.Password
		if spec.BasicAuth != nil {
			if err := validateBasicAuth(spec.BasicAuth, "procfile."+web.Name+".basic_auth"); err != nil {
				return err
			}
			web.BasicAuth = spec.BasicAuth
		}
		if spec.Password != nil {
			web.Password = *spec.Password
		}
	}
	for _, name := range processNames(a.Procfile) {
		spec := a.Procfile[name]
		for key, set := range map[string]bool{"basic_auth": spec.BasicAuth != nil, "password": spec.Password != nil} {
			if set && !a.IsWeb(name) {
				return &Error{Key: "procfile." + name + "." + key, Message: key + " is only valid on a web process", Hint: "declare host or subdomain on this process, or set " + key + " at the top level of the app file"}
			}
		}
	}
	return nil
}

// resolveHealth moves each web process's health path onto its process keys. Only a web process may
// set it, and the value is a path because dboss always talks HTTP to a process port.
func (a *App) resolveHealth() error {
	for _, name := range processNames(a.Procfile) {
		spec := a.Procfile[name]
		if spec.Health == "" {
			continue
		}
		if !a.IsWeb(name) {
			return &Error{Key: "procfile." + name + ".health", Message: "health is only valid on a web process", Hint: "declare host or subdomain on this process, or set health on a web process"}
		}
		if !strings.HasPrefix(spec.Health, "/") {
			return &Error{Key: "procfile." + name + ".health", Message: fmt.Sprintf("must be a path starting with /, not %q", spec.Health), Hint: "e.g. health: /up"}
		}
		override := a.Processes[name]
		path := spec.Health
		override.Health = &path
		a.Processes[name] = override
	}
	return nil
}

// resolvePubsub resolves every web process's pubsub option onto its WebProcess. A process that
// sets pubsub must declare a host or a subdomain so the hub has hosts to serve on.
func (a *App) resolvePubsub() error {
	for index := range a.WebProcesses {
		web := &a.WebProcesses[index]
		spec := a.Procfile[web.Name]
		if spec.Pubsub == nil || !spec.Pubsub.enabled() {
			continue
		}
		pubsub := defaultPubsub
		if spec.Pubsub.Path != "" {
			pubsub.Path = spec.Pubsub.Path
		}
		if settings := spec.Pubsub.Settings; settings != nil {
			apply(&pubsub, *settings)
		}
		if err := validatePubsub(pubsub, "procfile."+web.Name+".pubsub"); err != nil {
			return err
		}
		web.Pubsub = pubsub
	}
	for _, name := range processNames(a.Procfile) {
		spec := a.Procfile[name]
		if spec.Pubsub != nil && spec.Pubsub.enabled() && !a.IsWeb(name) {
			return &Error{Key: "procfile." + name + ".pubsub", Message: "pubsub needs a host or subdomain on the same process", Hint: "add host or subdomain to this process or move pubsub to a web process"}
		}
	}
	return nil
}

// resolveVibe resolves every web process's vibe option onto its WebProcess. Outside a dev session
// the web process needs a gate (password, basic_auth or auth), since the harness runs commands on
// the box and anyone the app lets in gets it.
func (a *App) resolveVibe(dev bool) error {
	for index := range a.WebProcesses {
		web := &a.WebProcesses[index]
		if !a.Procfile[web.Name].Vibe {
			continue
		}
		if !dev && web.Password == "" && len(web.BasicAuth) == 0 && len(a.Web.Auth) == 0 {
			return &Error{Key: "procfile." + web.Name + ".vibe", Message: "needs a gate outside a dev session", Hint: "the harness runs commands on the box: set password, basic_auth or auth on this app"}
		}
		web.Vibe = Vibe{Enabled: true}
	}
	for _, name := range processNames(a.Procfile) {
		if a.Procfile[name].Vibe && !a.IsWeb(name) {
			return &Error{Key: "procfile." + name + ".vibe", Message: "vibe needs a host or subdomain on the same process", Hint: "add host or subdomain to this process, or move vibe to a web process"}
		}
	}
	return nil
}

// resolveCanonical validates each web process's canonical_host against its own hosts and rejects
// canonical_host on a process that serves no hosts.
func (a *App) resolveCanonical() error {
	for _, web := range a.WebProcesses {
		if web.CanonicalHost != "" && !hostAllowed(web.CanonicalHost, web.Hosts) {
			return keyErr("procfile."+web.Name+".canonical_host", "%q is not one of the hosts %v", web.CanonicalHost, web.Hosts)
		}
	}
	for _, name := range processNames(a.Procfile) {
		spec := a.Procfile[name]
		if spec.CanonicalHost != "" && !a.IsWeb(name) {
			return &Error{Key: "procfile." + name + ".canonical_host", Message: "canonical_host is only valid on a web process", Hint: "declare host or subdomain on this process, or set canonical_host on a web process"}
		}
	}
	return nil
}

// UseDevHosts binds the first process to <app>.lvh.me when no process declares a host or a subdomain. It is
// single-app mode only, so a host running a folder with no config still gets a hostname.
func (a *App) UseDevHosts(app string) {
	if len(a.Hosts) > 0 || len(a.WebProcesses) > 0 || len(a.Procfile) == 0 {
		return
	}
	name := "web"
	if _, ok := a.Procfile["web"]; !ok {
		name = processNames(a.Procfile)[0]
	}
	hosts := List{"." + app + "." + DevDomain}
	a.WebProcesses = []WebProcess{{Name: name, Hosts: hosts, Static: string(a.Web.Static)}}
	a.Hosts = hosts
}

type appFile struct {
	Procfile  map[string]ProcessSpec      `yaml:"procfile"`
	Domain    string                      `yaml:"domain"`
	Branch    string                      `yaml:"branch"`
	Autostart Autostart                   `yaml:"autostart"`
	Deletable bool                        `yaml:"deletable"`
	Cron      map[string]CronJob          `yaml:"cron"`
	Hooks     map[string]Hook             `yaml:"hooks"`
	Lifecycle map[string]LifecycleCommand `yaml:"lifecycle"`
	Pages     string                      `yaml:"pages"`
	Overrides `yaml:",inline"`
	Processes map[string]ProcessOverrides `yaml:"processes"`
}

// LoadApp reads an app's dboss.yaml under a host. Host keys are rejected here because only the
// root file dboss start was pointed at owns the proxy, ports and runtime directories.
func LoadApp(path string, defaults Defaults) (App, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return App{}, err
	}
	return ParseApp(data, path, defaults)
}

// ParseApp is LoadApp on bytes already in memory.
func ParseApp(data []byte, path string, defaults Defaults) (App, error) {
	raw := file{Config: Default()}
	keys, root, err := decode(data, path, &raw, ProdSuffix)
	if err != nil {
		return App{}, err
	}
	for _, key := range hostKeys {
		if keys[key] {
			return App{}, located(keyErr(key, "is only valid in %s", ServerFileName), path, root)
		}
	}
	app, err := buildApp(raw.appFile, defaults, false, "")
	if err != nil {
		return App{}, located(err, path, root)
	}
	return app, nil
}

func buildApp(raw appFile, defaults Defaults, dev bool, name string) (App, error) {
	if len(raw.Procfile) == 0 {
		return App{}, &Error{Key: "procfile", Message: "must contain at least one process", Hint: "e.g. procfile:\n    web: bundle exec puma"}
	}
	app := App{Procfile: raw.Procfile, Domain: raw.Domain, Branch: raw.Branch, Autostart: AutostartOn, Deletable: raw.Deletable, Cron: raw.Cron, Hooks: raw.Hooks, Lifecycle: raw.Lifecycle, Pages: DefaultPages, Defaults: defaults, Processes: raw.Processes}
	if raw.Pages != "" {
		app.Pages = raw.Pages
	}
	if err := app.deriveWeb(); err != nil {
		return App{}, err
	}
	if raw.Autostart != "" {
		app.Autostart = raw.Autostart
	}
	for name := range app.Processes {
		if _, ok := app.Procfile[name]; !ok {
			return App{}, keyErr("processes."+name, "is not in procfile")
		}
	}
	if app.Processes == nil {
		app.Processes = map[string]ProcessOverrides{}
	}
	apply(&app.Defaults, raw.Overrides)
	if dev {
		app.UseDevHosts(name)
	}
	if err := app.resolveHosts(dev); err != nil {
		return App{}, err
	}
	app.resolveStatic()
	if err := app.resolveAccess(); err != nil {
		return App{}, err
	}
	if err := app.resolveHealth(); err != nil {
		return App{}, err
	}
	if err := app.resolvePubsub(); err != nil {
		return App{}, err
	}
	if err := app.resolveVibe(dev); err != nil {
		return App{}, err
	}
	if err := app.resolveCanonical(); err != nil {
		return App{}, err
	}
	for _, web := range app.WebProcesses {
		if web.Pubsub.Enabled() && app.AuthCog.Enabled() && web.Pubsub.Path == string(app.AuthCog) {
			return App{}, &Error{Key: "procfile." + web.Name + ".pubsub", Message: fmt.Sprintf("path %q collides with authcog", web.Pubsub.Path)}
		}
	}
	if err := validateDefaults(app.Defaults); err != nil {
		return App{}, err
	}
	if err := validateCron(app.Cron); err != nil {
		return App{}, err
	}
	if err := validateHooks(app.Hooks); err != nil {
		return App{}, err
	}
	if err := validateLifecycle(app.Lifecycle); err != nil {
		return App{}, err
	}
	if err := validateBranch(app.Branch); err != nil {
		return App{}, err
	}
	app.allowPrefixes, _ = parsePrefixes(app.AllowIPs)
	for name := range app.Processes {
		if err := validateProcess(app.Process(name)); err != nil {
			return App{}, scoped(err, "processes."+name)
		}
	}
	return app, nil
}

// Process returns the process keys for name with its processes.<name> overrides applied.
func (a App) Process(name string) Process {
	p := a.Defaults.Process
	if o, ok := a.Processes[name]; ok {
		apply(&p, o)
	}
	return p
}
