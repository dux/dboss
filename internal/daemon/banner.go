package daemon

import (
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"

	"dboss/internal/config"
	"dboss/internal/supervisor"
	"dboss/internal/version"
)

// Banner is a session's startup summary: the intro line, one row per process, and whether a dev
// session serves HTTPS. A dev `dboss start` reads it over the control socket when it restarts the
// session already running, so both print the same summary.
type Banner struct {
	Intro string      `json:"intro"`
	Rows  []BannerRow `json:"rows"`
	HTTPS bool        `json:"https"`
}

// Lines renders the banner with echo's keys, so each row shares its process's log color.
func (b Banner) Lines(echo *supervisor.Echo) []string {
	return append([]string{b.Intro}, renderBanner(b.Rows, echo)...)
}

// Banner builds the summary from the bound listeners and the live snapshots.
func (d *Daemon) Banner() Banner {
	scheme, port := d.appAddress()
	console, note := "", ""
	// A dev or hand-run session admits loopback without a token, so the console row carries no
	// sign-in link.
	if d.management != nil {
		console, note = "http://"+managementAddress(d.managementPort), "open on this machine"
		if !d.cfg.Dev() && !d.cfg.Local {
			note = "run `dboss login` to sign in"
		}
	}
	secure := devHTTPS{}
	if d.devHTTPS != "" {
		secure = devHTTPS{port: bannerPort(d.devHTTPS, "https"), note: "run `dboss trust` once so the browser accepts it"}
		if d.devTLS.Trusted() {
			secure.note = "trusted local certificate"
		}
	}
	return Banner{Intro: bannerIntro(d.cfg), Rows: banner(d.manager.Snapshots(), console, note, scheme, port, secure), HTTPS: d.devHTTPS != ""}
}

// printBanner writes the startup summary of a hand-run session. It is terminal-only, the same
// signal that drives the output echo and the privileged-port fallback, so a service start still
// logs nothing extra and never mints a console link.
func (d *Daemon) printBanner() {
	if d.echo == nil || len(d.listen) == 0 {
		return
	}
	for _, line := range d.Banner().Lines(d.echo) {
		d.echo.Print(line)
	}
}

// appAddress is the scheme and port a browser reaches the apps on through the proxy. A dev
// session keeps plain http on proxy.listen; its HTTPS gets its own banner row. The scheme is
// empty when no proxy is listening.
func (d *Daemon) appAddress() (scheme, port string) {
	if len(d.listen) == 0 {
		return "", ""
	}
	if d.cfg.Proxy.TLS.Enabled() && !d.cfg.Dev() {
		return "https", bannerPort(d.cfg.Proxy.TLS.Listen, "https")
	}
	return "http", bannerPort(d.listen[0], "http")
}

// appLinkAddress is the address `dboss ls` and the console link an app with. It is the banner's
// appAddress but prefers a dev session's HTTPS listener, so a browser opens a trusted https URL
// instead of the plain http one on :80. A host session is unchanged.
func (d *Daemon) appLinkAddress() (scheme, port string) {
	if d.cfg.Dev() && d.devHTTPS != "" {
		return "https", bannerPort(d.devHTTPS, "https")
	}
	return d.appAddress()
}

// bannerIntro names the build and what this session runs, above the rows.
func bannerIntro(cfg config.Config) string {
	if cfg.Dev() {
		return fmt.Sprintf("dboss %s - dev session for %s:", version.String(), filepath.Base(cfg.Dir))
	}
	return fmt.Sprintf("dboss %s - host %s:", version.String(), cfg.Dir)
}

// banner is the startup summary a hand-run session prints before any app starts: one row per
// process, keyed by the same colored prefix that process logs under, so the address and its later
// output line up. Web processes carry a clickable URL and workers say so.
func banner(snapshots []supervisor.Snapshot, console, consoleNote string, scheme, port string, secure devHTTPS) []BannerRow {
	rows := make([]BannerRow, 0, len(snapshots))
	if console != "" {
		// A sign-in link carries a token, so it is far longer than any hostname. Keeping it out
		// of the column width stops one row from stretching every other one.
		rows = append(rows, newRow("dboss", "console", console, consoleNote, false))
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Name < snapshots[j].Name })
	secureURL := ""
	for _, app := range snapshots {
		web := map[string]bool{}
		for _, process := range app.WebProcesses {
			web[process.Name] = true
			rows = append(rows, newRow(app.Name, process.Name, webURL(process, scheme, port), bannerNote(app), true))
			if secureURL == "" && config.PrimaryHost(process.CanonicalHost, process.Hosts) != "" {
				secureURL = webURL(process, "https", secure.port)
			}
		}
		for _, process := range app.Processes {
			if web[process.Type] {
				continue
			}
			rows = append(rows, newRow(app.Name, process.Name, "worker", bannerNote(app), true))
		}
	}
	if secure.note != "" && secureURL != "" {
		rows = append(rows, newRow("dboss", "https", secureURL, secure.note, true))
	}
	return rows
}

// renderBanner turns the rows into lines, every address padded to one column.
func renderBanner(rows []BannerRow, echo *supervisor.Echo) []string {
	// Register every name before rendering any key, so all rows pad to the widest one.
	for _, row := range rows {
		echo.Key(row.App, row.Proc)
	}
	addressWidth := 0
	for _, row := range rows {
		if row.Pad {
			addressWidth = max(addressWidth, len(row.Address))
		}
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		address := row.Address
		if row.Pad {
			address = fmt.Sprintf("%-*s", addressWidth, address)
		}
		lines = append(lines, strings.TrimRight(echo.Key(row.App, row.Proc)+address+"  "+row.Note, " "))
	}
	return lines
}

// devHTTPS is the dev session's HTTPS listener as the banner shows it: the port to print and
// whether the browser will trust it. A zero value prints nothing.
type devHTTPS struct {
	port string
	note string
}

// BannerRow is one line: the process it names, its address and a note. Pad lines the address up
// with the other rows.
type BannerRow struct {
	App     string `json:"app"`
	Proc    string `json:"proc"`
	Address string `json:"address"`
	Note    string `json:"note,omitempty"`
	Pad     bool   `json:"pad,omitempty"`
}

func newRow(app, proc, address, note string, pad bool) BannerRow {
	return BannerRow{App: app, Proc: proc, Address: address, Note: note, Pad: pad}
}

// webURL is the address to open for one web process. A pattern that matches only subdomains has
// no address of its own, so it is printed as written instead of being turned into a link that
// would not resolve.
func webURL(web supervisor.WebProcessSnapshot, scheme, port string) string {
	if url := config.WebURL(scheme, port, web.CanonicalHost, web.Hosts); url != "" {
		return url
	}
	return strings.Join(web.Hosts, ", ")
}

// bannerNote is what a process row adds after its address. The banner prints before any app
// starts, so the state would always read stopped; only maintenance changes what a click shows.
func bannerNote(app supervisor.Snapshot) string {
	if app.Maintenance {
		return "maintenance"
	}
	return ""
}

// bannerPort is the port to put in a printed URL: the one actually bound, dropped when it is
// the default for the scheme and the browser would add it back.
func bannerPort(address, scheme string) string {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return ""
	}
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		return ""
	}
	return port
}
