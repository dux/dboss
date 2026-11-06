package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"dboss/internal/cloudflare"
	"dboss/internal/config"
)

func (c CLI) cf(args []string) error {
	if len(args) == 0 || args[0] == "help" {
		return c.help(c.Out, "cf")
	}
	switch args[0] {
	case "dns":
		return c.cfDNS(args[1:])
	default:
		return fmt.Errorf("unknown cf subcommand %q (use dns)", args[0])
	}
}

// cfDNS points every hostname the app's web processes answer at its server on Cloudflare. It
// prints the plan and changes nothing unless --set is given; records are created or updated in
// place, never deleted.
func (c CLI) cfDNS(args []string) error {
	set := flag.NewFlagSet("cf dns", flag.ContinueOnError)
	set.SetOutput(c.Err)
	configPath := configFlag(set)
	apply := set.Bool("set", false, "create and update the records")
	ip := set.String("ip", "", "address the records point at (default: asked from the server over ssh)")
	dnsOnly := set.Bool("dns-only", false, "unproxied records (the box terminates TLS itself)")
	force := set.Bool("force", false, "replace a CNAME at an app hostname")
	token := set.String("token", "", "Cloudflare API token (default: $"+cloudflare.TokenEnv+")")
	operands, err := parseSubcommandFlags(set, args)
	if err != nil {
		return err
	}
	if len(operands) != 0 {
		return errors.New("usage: dboss cf dns [--set] [--ip addr] [--dns-only] [--force] [--token t]")
	}
	here := &workdir{explicit: *configPath}
	configFile, cfg, err := here.load()
	if err != nil {
		return err
	}
	if cfg.App == nil {
		return fmt.Errorf("%s is a host config; run cf dns in an app folder", configFile)
	}
	// The hostnames the box serves: the shipped file, read the way the box reads it.
	shipped, err := config.LoadApp(filepath.Join(filepath.Dir(configFile), config.FileName), config.Default().Defaults)
	if err != nil {
		return err
	}
	names, skipped := dnsNames(shipped)
	for _, pattern := range skipped {
		fmt.Fprintf(c.Err, "skipped %s: no DNS record can express it\n", pattern)
	}
	if len(names) == 0 {
		return errors.New("no web process declares a host or a subdomain, so there is nothing to point at the server")
	}
	address := *ip
	if address == "" {
		target := cfg.App.ServerTarget(filepath.Base(cfg.Dir))
		if target == "" {
			return errors.New("no server: set server in dboss.yaml or pass --ip")
		}
		host, _, err := config.SplitTarget(target)
		if err != nil {
			return err
		}
		if address, err = remoteIP(host); err != nil {
			return err
		}
	}
	parsed := net.ParseIP(address)
	if parsed == nil {
		return fmt.Errorf("%q is not an IP address", address)
	}
	recordType := "AAAA"
	if v4 := parsed.To4(); v4 != nil {
		recordType, parsed = "A", v4
	}
	client := cloudflare.Client{Token: orElse(*token, os.Getenv(cloudflare.TokenEnv))}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	changes, zones, err := planDNS(ctx, client, names, cloudflare.Record{Type: recordType, Content: parsed.String(), Proxied: !*dnsOnly, TTL: cloudflare.AutoTTL}, *force)
	if err != nil {
		return err
	}
	writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME\tTYPE\tCURRENT\tWANTED\tACTION")
	pending, conflicts := 0, 0
	for _, change := range changes {
		action := string(change.Action)
		switch change.Action {
		case cloudflare.ActionCreate, cloudflare.ActionUpdate:
			pending++
		case cloudflare.ActionConflict:
			conflicts++
			action += ": " + change.Reason
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", change.Want.Name, change.Want.Type, describeRecord(change.Current), describeRecord(&change.Want), action)
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	if !*apply {
		if pending > 0 {
			fmt.Fprintf(c.Out, "%d %s to apply; run with --set\n", pending, plural(pending, "change"))
		}
		return nil
	}
	for _, change := range changes {
		var err error
		switch change.Action {
		case cloudflare.ActionCreate:
			_, err = client.Create(ctx, zones[change.Want.Name], change.Want)
		case cloudflare.ActionUpdate:
			_, err = client.Update(ctx, zones[change.Want.Name], change.Want)
		default:
			continue
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(c.Out, "%sd %s %s -> %s\n", change.Action, change.Want.Type, change.Want.Name, change.Want.Content)
	}
	if conflicts > 0 {
		return fmt.Errorf("%d %s not set, see the conflict %s above", conflicts, plural(conflicts, "name"), plural(conflicts, "row"))
	}
	return nil
}

// planDNS looks up each name's zone and records and plans its change. zones maps a name to its
// zone id; a zone found once covers every later name under it. A name no zone holds becomes a
// conflict row, so the rest of the app still gets its records.
func planDNS(ctx context.Context, client cloudflare.Client, names []string, want cloudflare.Record, force bool) ([]cloudflare.Change, map[string]string, error) {
	var known []cloudflare.Zone
	zoneOf := func(name string) (cloudflare.Zone, error) {
		best := -1
		for index, zone := range known {
			if (name == zone.Name || strings.HasSuffix(name, "."+zone.Name)) && (best < 0 || len(zone.Name) > len(known[best].Name)) {
				best = index
			}
		}
		if best >= 0 {
			return known[best], nil
		}
		zone, err := client.ZoneFor(ctx, name)
		if err == nil {
			known = append(known, zone)
		}
		return zone, err
	}
	changes := make([]cloudflare.Change, 0, len(names))
	zones := make(map[string]string, len(names))
	for _, name := range names {
		record := want
		record.Name = name
		zone, err := zoneOf(name)
		if errors.Is(err, cloudflare.ErrNoZone) {
			changes = append(changes, cloudflare.Change{Want: record, Action: cloudflare.ActionConflict, Reason: "no zone this token can read holds it"})
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		existing, err := client.Records(ctx, zone.ID, name)
		if err != nil {
			return nil, nil, err
		}
		zones[name] = zone.ID
		changes = append(changes, cloudflare.Plan(record, existing, force))
	}
	return changes, zones, nil
}

// dnsNames turns the app's host patterns into record names, in order and without repeats: a
// hostname is itself, "*.x" is the wildcard record, ".x" is x plus its wildcard. A pattern DNS
// cannot express ("mail.*", a "*" inside a label) and an IP literal are returned as skipped.
func dnsNames(app config.App) (names, skipped []string) {
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, web := range app.WebProcesses {
		for _, host := range web.Hosts {
			pattern := config.NormalizePattern(host)
			switch {
			case strings.HasPrefix(pattern, ".") && !strings.Contains(pattern, "*"):
				add(pattern[1:])
				add("*" + pattern)
			case strings.HasPrefix(pattern, "*.") && !strings.Contains(pattern[2:], "*"):
				add(pattern)
			case strings.Contains(pattern, "*") || net.ParseIP(pattern) != nil || !strings.Contains(pattern, "."):
				skipped = append(skipped, host)
			default:
				add(pattern)
			}
		}
	}
	return names, skipped
}

func describeRecord(record *cloudflare.Record) string {
	if record == nil {
		return "-"
	}
	mode := "proxied"
	if !record.Proxied {
		mode = "dns only"
	}
	if record.Type != "A" && record.Type != "AAAA" {
		return record.Type + " " + record.Content
	}
	return record.Content + " (" + mode + ")"
}

// remoteIP asks the box for its public address, the same one its Sys tab shows.
func remoteIP(host string) (string, error) {
	command := exec.Command("ssh", host, "dboss", "deploy", "ip")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("ssh %s dboss deploy ip: %w: %s (pass --ip to skip asking)", host, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
