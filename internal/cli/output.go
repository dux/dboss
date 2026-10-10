package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"dboss/internal/config"
	"dboss/internal/ctl"
	"dboss/internal/humanize"
	"dboss/internal/logstore"
	"dboss/internal/ops"
	"dboss/internal/pg"
	"dboss/internal/pubsub"
	"dboss/internal/supervisor"
)

// printKeys lists the documented config keys, grouped by block. filter narrows by a substring
// of the key path.
func (c CLI) printKeys(filter string, jsonOutput bool) error {
	var keys []config.Key
	for _, key := range config.Keys() {
		if filter == "" || strings.Contains(key.Path, filter) {
			keys = append(keys, key)
		}
	}
	if jsonOutput {
		encoded, _ := json.MarshalIndent(keys, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
		return nil
	}
	if len(keys) == 0 {
		return fmt.Errorf("no config key matches %q", filter)
	}
	writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
	for _, block := range config.Blocks() {
		first := true
		for _, key := range keys {
			if key.Block != block.ID {
				continue
			}
			if first {
				fmt.Fprintf(writer, "%s  (%s)\n", block.Title, blockNote(block.Scope))
				first = false
			}
			value := key.Default
			if value == "" {
				value = "e.g. " + key.Example
			} else if key.Example != "" {
				value = value + "  e.g. " + key.Example
			}
			// The last text on a line is not a padded cell, so lines without the scope
			// column end right after the value instead of trailing spaces.
			if key.PerProcess {
				fmt.Fprintf(writer, "  %s\t%s\t%s\tper process\n", key.Path, key.Description, value)
			} else {
				fmt.Fprintf(writer, "  %s\t%s\t%s\n", key.Path, key.Description, value)
			}
		}
		if !first {
			fmt.Fprintln(writer)
		}
	}
	return writer.Flush()
}

// blockNote says which file a block belongs in, printed next to the block title.
func blockNote(scope config.Scope) string {
	switch scope {
	case config.ScopeService:
		return "dboss-server.yaml"
	case config.ScopeApp:
		return "app dboss.yaml"
	default:
		return "defaults: in dboss-server.yaml, top level in an app file; per-process ones also under processes.<name>"
	}
}

// printHosts lists one row per web process with every hostname it answers, the canonical one
// first. Workers have no hosts and are not listed.
func (c CLI) printHosts(snapshots []supervisor.Snapshot, app string) error {
	type row struct{ app, process, hosts string }
	var rows []row
	for _, snapshot := range snapshots {
		if app != "" && snapshot.Name != app {
			continue
		}
		for _, web := range snapshot.WebProcesses {
			hosts := slices.Clone(web.Hosts)
			if web.CanonicalHost != "" {
				hosts = slices.DeleteFunc(hosts, func(host string) bool { return host == web.CanonicalHost })
				hosts = append([]string{web.CanonicalHost}, hosts...)
			}
			rows = append(rows, row{snapshot.Name, web.Name, strings.Join(hosts, ", ")})
		}
	}
	if len(rows) == 0 {
		switch {
		case app != "":
			fmt.Fprintf(c.Out, "app %q has no web processes\n", app)
		default:
			fmt.Fprintln(c.Out, "no web processes")
		}
		return nil
	}
	writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "APP\tPROCESS\tHOSTS")
	for _, r := range rows {
		fmt.Fprintf(writer, "%s\t%s\t%s\n", r.app, r.process, r.hosts)
	}
	return writer.Flush()
}

func (c CLI) printHuman(method string, data any) error {
	switch method {
	case ops.ActionList:
		writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "NAME\tSTATE\tPORT\tURL\tUPTIME\tLAST ACTIVITY\tMEM (APPROX)\tPID")
		for _, snapshot := range data.([]supervisor.Snapshot) {
			for index, process := range snapshot.Processes {
				state := string(process.State)
				// Draining and maintenance are app-wide, so they ride the app's first row.
				if index == 0 {
					if snapshot.Draining {
						state += " draining"
					}
					if snapshot.Maintenance {
						state += " maintenance"
					}
				}
				port, pid := "-", "-"
				if process.Port > 0 {
					port = strconv.Itoa(process.Port)
				}
				if process.PID > 0 {
					pid = strconv.Itoa(process.PID)
				}
				uptime, memory := "-", "-"
				if process.State == supervisor.Running {
					memory = humanize.Bytes(process.MemoryBytes)
					if !process.StartedAt.IsZero() {
						uptime = humanize.Duration(time.Since(process.StartedAt))
					}
				}
				last := ""
				if index == 0 {
					last = humanize.Ago(snapshot.LastActivity)
				}
				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", snapshot.Name+"/"+process.Name, state, port, processURL(snapshot, process), uptime, last, memory, pid)
			}
		}
		return writer.Flush()
	case ops.ActionStatus:
		encoded, _ := json.MarshalIndent(data, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
	case ops.ActionLogs:
		logs := data.(map[string][]string)
		names := slices.Sorted(maps.Keys(logs))
		for _, name := range names {
			for _, line := range logs[name] {
				fmt.Fprintf(c.Out, "[%s] %s\n", name, line)
			}
		}
	case ops.ActionLogSearch:
		rows := data.([]logstore.LogEntry)
		if len(rows) == 0 {
			fmt.Fprintln(c.Out, "no matching log rows")
			return nil
		}
		for _, row := range rows {
			fmt.Fprintf(c.Out, "%s %-5s %-12s %s\n", row.Time.Local().Format("2006-01-02 15:04:05"), strings.ToUpper(row.Level), row.Process, row.Message)
		}
	case ops.ActionPorts:
		entries := data.(map[string]int)
		names := slices.Sorted(maps.Keys(entries))
		for _, name := range names {
			fmt.Fprintf(c.Out, "%s\t%d\n", name, entries[name])
		}
	case ops.ActionCron:
		jobs := data.([]supervisor.CronSnapshot)
		if len(jobs) == 0 {
			fmt.Fprintln(c.Out, "no scheduled jobs")
			return nil
		}
		writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "JOB\tSCHEDULE\tNEXT\tLAST\tCOMMAND")
		for _, job := range jobs {
			next := "-"
			if !job.Next.IsZero() {
				next = job.Next.Format("2006-01-02 15:04")
			}
			last := "-"
			switch {
			case job.Running:
				last = "running"
			case job.LastError != "":
				last = job.LastError
			case !job.LastEnd.IsZero():
				last = fmt.Sprintf("exit %d at %s", job.LastExit, job.LastEnd.Format("15:04"))
			}
			name := job.Name
			if job.Disabled {
				name += " (disabled)"
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", name, job.Schedule, next, last, job.Command)
		}
		return writer.Flush()
	case ops.ActionHook:
		hooks := data.([]supervisor.HookInfo)
		if len(hooks) == 0 {
			fmt.Fprintln(c.Out, "no hooks")
			return nil
		}
		writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "HOOK\tLAST\tCOMMAND\tURL")
		missing := false
		for _, hook := range hooks {
			last := "-"
			switch {
			case hook.Running:
				last = "running"
			case hook.LastError != "":
				last = hook.LastError
			case !hook.LastEnd.IsZero():
				last = fmt.Sprintf("exit %d at %s", hook.LastExit, hook.LastEnd.Format("15:04"))
			}
			name := hook.Name
			if hook.Disabled {
				name += " (disabled)"
			}
			link := hook.URL
			if link == "" {
				link, missing = "-", true
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", name, last, hook.Command, link)
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		if missing {
			fmt.Fprintln(c.Out, "no ping URL: set tokens.dboss and management.host in dboss-server.yaml")
		}
	case ops.ActionEvents, ops.ActionEventsLatest, ops.ActionEventsFacets, ops.ActionEventsViews, ops.ActionEventsFunnel, ops.ActionEventsQuery:
		return c.printEvents(method, data)
	case ops.ActionAudit:
		rows := data.([]logstore.AuditEntry)
		if len(rows) == 0 {
			fmt.Fprintln(c.Out, "no audit rows")
			return nil
		}
		writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "TIME\tACTOR\tACTION\tAPP\tDETAIL\tRESULT")
		for _, row := range rows {
			result := row.Result
			if row.Error != "" {
				result += ": " + row.Error
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", row.Time.Local().Format("2006-01-02 15:04:05"), row.Actor, row.Action, row.App, row.Detail, result)
		}
		return writer.Flush()
	case ops.ActionRescan:
		result := data.(ops.RescanResult)
		fmt.Fprintf(c.Out, "rescan complete (%d invalid)\n", len(result.Invalid))
		for _, message := range result.Invalid {
			fmt.Fprintf(c.Out, "  %s\n", message)
		}
		if len(result.RestartRequired) > 0 {
			fmt.Fprintf(c.Out, "restart required: %s changed (systemctl restart dboss, or Ctrl-C and dboss start)\n", strings.Join(result.RestartRequired, ", "))
		}
	case ops.ActionAdd:
		snapshot := data.(supervisor.Snapshot)
		fmt.Fprintf(c.Out, "added %s (%s) in %s\n", snapshot.Name, snapshot.State, snapshot.Dir)
		for _, web := range snapshot.WebProcesses {
			if host := config.PrimaryHost(web.CanonicalHost, web.Hosts); host != "" {
				fmt.Fprintf(c.Out, "  %s  https://%s\n", web.Name, host)
			}
		}
	case ctl.LoginMethod:
		links := data.(map[string]string)
		fmt.Fprintf(c.Out, "local:  %s\n", links["url"])
		if public := links["public_url"]; public != "" {
			fmt.Fprintf(c.Out, "public: %s\n", public)
		}
		fmt.Fprintln(c.Out, "Opens the console as cli@localhost. Valid for 3 minutes, one use.")
	case ops.ActionPG:
		snapshot := data.(pg.Snapshot)
		summary := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintf(summary, "server\t%s\n", snapshot.Server.Version)
		fmt.Fprintf(summary, "connection\t%s\n", snapshot.Server.Description)
		fmt.Fprintf(summary, "uptime\t%d seconds\n", snapshot.Server.UptimeSeconds)
		fmt.Fprintf(summary, "connections\t%d of %d\n", snapshot.Server.CurrentConnections, snapshot.Server.MaxConnections)
		fmt.Fprintf(summary, "activity\t%d active, %d idle, %d blocked\n", snapshot.Activity.Active, snapshot.Activity.Idle, snapshot.Activity.Blocked)
		if err := summary.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(c.Out)
		writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "DATABASE\tSIZE\tOWNER\tCONNS\tBACKUP\tROTATION")
		for _, database := range snapshot.Databases {
			fmt.Fprintf(writer, "%s\t%d\t%s\t%d\t%t\t%s\n", database.Name, database.SizeBytes, database.Owner, database.Connections, database.BackupSelected, database.BackupRotation)
		}
		return writer.Flush()
	case ops.ActionPGBackups:
		backups := data.([]pg.Backup)
		if len(backups) == 0 {
			fmt.Fprintln(c.Out, "no backups recorded")
			return nil
		}
		writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "DATABASE\tTIME\tSIZE\tMANUAL\tSTATUS\tID")
		for _, entry := range backups {
			fmt.Fprintf(writer, "%s\t%s\t%d\t%t\t%s\t%s\n", entry.Database, entry.Time, entry.Bytes, entry.Manual, entry.Status, entry.ID)
		}
		return writer.Flush()
	case ops.ActionPGBackup:
		backups := data.([]pg.Backup)
		for _, entry := range backups {
			if entry.Error != "" {
				fmt.Fprintf(c.Out, "%s: failed: %s\n", entry.Database, entry.Error)
				continue
			}
			fmt.Fprintf(c.Out, "%s: %d bytes in %dms\n", entry.Database, entry.Bytes, entry.DurationMS)
		}
	case ops.ActionPGRestore:
		result := data.(pg.RestoreResult)
		fmt.Fprintf(c.Out, "restored %d bytes into %s\n", result.Bytes, result.Target)
	case ops.ActionPGDrop:
		fmt.Fprintf(c.Out, "dropped %s\n", data.(string))
	case ops.ActionPGDeleteDump:
		fmt.Fprintf(c.Out, "deleted backup %s\n", data.(string))
	case ops.ActionPubsub:
		apps := data.([]pubsub.App)
		if len(apps) == 0 {
			fmt.Fprintln(c.Out, "no app serves realtime channels (set pubsub on an app's web process)")
			return nil
		}
		writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "APP\tPROCESS\tPATH\tCLIENTS\tCHANNELS")
		for _, app := range apps {
			channels := "-"
			if len(app.Channels) > 0 {
				parts := make([]string, len(app.Channels))
				for i, channel := range app.Channels {
					parts[i] = fmt.Sprintf("%s(%d)", channel.Name, channel.Subscribers)
				}
				channels = strings.Join(parts, ",")
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\n", app.Name, app.Process, app.Path, app.Clients, channels)
		}
		return writer.Flush()
	case ops.ActionPubsubSecret, ops.ActionPubsubRotate:
		info := data.(ops.PubsubSecret)
		if method == ops.ActionPubsubRotate {
			fmt.Fprintln(c.Out, "rotated; new publish secret:")
		}
		fmt.Fprintf(c.Out, "app:       %s\n", info.App)
		fmt.Fprintf(c.Out, "process:   %s\n", info.Process)
		fmt.Fprintf(c.Out, "path:      %s\n", info.Path)
		fmt.Fprintf(c.Out, "secret:    %s\n", info.Secret)
		fmt.Fprintf(c.Out, "subscribe: %s\n", info.Subscribe)
		fmt.Fprintf(c.Out, "publish:   %s\n", info.Publish)
	case ops.ActionPubsubPublish:
		result := data.(ops.PubsubPublished)
		fmt.Fprintf(c.Out, "published to %s (%d subscribers)\n", result.Channel, result.Subscribers)
	default:
		fmt.Fprintln(c.Out, "ok")
	}
	return nil
}

// processURL is the address of one process: the URL ops built for its procfile entry, or "-" for
// a worker, a wildcard-only host set, or a session with no proxy listening.
func processURL(snapshot supervisor.Snapshot, process supervisor.ProcessSnapshot) string {
	for _, entry := range snapshot.URLs {
		if entry.Process == process.Type {
			return entry.URL
		}
	}
	return "-"
}
