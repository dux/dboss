package ops

// Param is one request field an action reads. Name is the Request JSON key.
type Param struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	Desc     string `json:"desc"`
}

// Spec documents one action for the HTTP API help and the OpenAPI export.
type Spec struct {
	Name    string  `json:"name"`
	Group   string  `json:"group"`
	Summary string  `json:"summary"`
	Params  []Param `json:"params,omitempty"`
	Audited bool    `json:"audited"`
}

// Param types. Duration is a Go duration string ("30s", "2m") on the HTTP API.
const (
	TypeString   = "string"
	TypeInteger  = "integer"
	TypeBoolean  = "boolean"
	TypeStrings  = "string[]"
	TypeObject   = "object"
	TypeJSON     = "json"
	TypeDuration = "duration"
)

func req(name, kind, desc string) Param {
	return Param{Name: name, Type: kind, Required: true, Desc: desc}
}

func opt(name, kind, desc string) Param {
	return Param{Name: name, Type: kind, Desc: desc}
}

var (
	pApp     = req("app", TypeString, "app name")
	pProcess = opt("process", TypeString, "procfile process (every instance) or one instance such as web.2; empty acts on the whole app")
	pQuery   = opt("query", TypeString, "event filter, e.g. `signup plan=pro since=7d`")
)

// specs is every action Do dispatches, in the order the API help lists them.
var specs = []Spec{
	{Name: ActionList, Group: "Apps", Summary: "List every app with its state, processes, request rates and disk usage."},
	{Name: ActionStatus, Group: "Apps", Summary: "One app's snapshot: state, processes, hosts, cron, rates and disk usage.", Params: []Param{pApp}},
	{Name: ActionStart, Group: "Apps", Summary: "Start an app, or one process of a running app.", Params: []Param{pApp, pProcess}},
	{Name: ActionStop, Group: "Apps", Summary: "Stop an app (draining in-flight requests first), or one process of it.", Params: []Param{pApp, pProcess}},
	{Name: ActionRestart, Group: "Apps", Summary: "Restart an app or one process. A running web process rolls with no downtime.", Params: []Param{pApp, pProcess}},
	{Name: ActionDestroy, Group: "Apps", Summary: "Stop an app, run its destroy lifecycle step and delete its folder. Cannot be undone.", Params: []Param{pApp}},
	{Name: ActionMaintenance, Group: "Apps", Summary: "Turn maintenance mode on or off; the proxy answers with the maintenance page while it is on.", Params: []Param{pApp, opt("on", TypeBoolean, "true turns maintenance on, false or missing turns it off")}},
	{Name: ActionRescan, Group: "Apps", Summary: "Reload every config file from disk and apply the app-level changes."},
	{Name: ActionAdd, Group: "Apps", Summary: "Clone a git repository that carries a dboss.yaml into the apps folder and start it.", Params: []Param{
		req("repo", TypeString, "git URL: https, ssh, scp form, host/path or owner/repo (GitHub)"),
		opt("app", TypeString, "app name; defaults to the repository name"),
		opt("branch", TypeString, "branch to check out; defaults to the remote default"),
		opt("host", TypeString, "hostname that replaces the app's own (one web process only)"),
	}},
	{Name: ActionPorts, Group: "Apps", Summary: "Port of every process slot, keyed app/slot."},
	{Name: ActionExec, Group: "Apps", Summary: "Run a one-off command in the app's folder and environment; returns combined output and exit code.", Params: []Param{
		pApp,
		req("argv", TypeStrings, "command and its arguments, run without a shell"),
		opt("timeout", TypeDuration, "kill after this long; default 60s"),
	}},

	{Name: ActionGitCommit, Group: "Git", Summary: "Stage every change in the app's checkout (untracked files too, ignored ones never) and commit it.", Params: []Param{pApp, req("message", TypeString, "commit message")}},
	{Name: ActionGitPush, Group: "Git", Summary: "Push the app's branch to its upstream with tokens.github; a pinned branch refuses any other.", Params: []Param{pApp}},
	{Name: ActionGitReset, Group: "Git", Summary: "Clear every uncommitted change by stashing it (untracked files too, ignored ones never); `git stash pop` brings it back.", Params: []Param{pApp}},

	{Name: ActionCron, Group: "Cron and hooks", Summary: "An app's cron jobs with their schedule and last run.", Params: []Param{pApp}},
	{Name: ActionCronRun, Group: "Cron and hooks", Summary: "Run one cron job now.", Params: []Param{pApp, req("job", TypeString, "cron job name")}},
	{Name: ActionHook, Group: "Cron and hooks", Summary: "An app's deploy hooks with their ping URL, state and last output.", Params: []Param{pApp}},
	{Name: ActionHookRun, Group: "Cron and hooks", Summary: "Run one deploy hook now (a restart: true hook restarts the app after a clean exit).", Params: []Param{pApp, req("hook", TypeString, "hook name")}},
	{Name: ActionHostHookRun, Group: "Cron and hooks", Summary: "Run a host-level hook; github_pr deploys or removes a pull request preview.", Params: []Param{
		req("hook", TypeString, "host hook name (github_pr)"),
		opt("params", TypeObject, "string map the hook reads, keyed by QS_ name (QS_BRANCH, QS_REPO, QS_ACTION, QS_NUM)"),
	}},

	{Name: ActionLogs, Group: "Logs and audit", Summary: "Tail of each process log, keyed by process.", Params: []Param{pApp, opt("process", TypeString, "one process; empty returns all"), opt("lines", TypeInteger, "lines per process")}},
	{Name: ActionLogSearch, Group: "Logs and audit", Summary: "Search the app's log store (process output, app log files, dboss's own log).", Params: []Param{
		pApp,
		opt("channel", TypeString, "stdout, dboss or file; `stdout:<process>` or `file:<log path>` narrows to one"),
		opt("process", TypeString, "process name"),
		opt("level", TypeString, "exact level: debug, info, warn or error"),
		opt("query", TypeString, "full-text search"),
		opt("lines", TypeInteger, "row limit; default 200"),
	}},
	{Name: ActionAudit, Group: "Logs and audit", Summary: "Newest audit rows (who ran which action), newest first.", Params: []Param{
		opt("app", TypeString, "only this app"),
		opt("by_actor", TypeString, "only this actor (an email, cli, api or `hook:<app>/<hook>`)"),
		opt("action", TypeString, "only this action name"),
		opt("lines", TypeInteger, "row limit"),
	}},
	{Name: ActionExceptionResolve, Group: "Logs and audit", Summary: "Mark an exception group resolved, or reopen it.", Params: []Param{pApp, req("exp_uid", TypeString, "exception fingerprint"), opt("on", TypeBoolean, "true resolves, false reopens")}},
	{Name: ActionExceptionIgnore, Group: "Logs and audit", Summary: "Ignore an exception group (it stays resolved), or stop ignoring it.", Params: []Param{pApp, req("exp_uid", TypeString, "exception fingerprint"), opt("on", TypeBoolean, "true ignores, false stops ignoring")}},
	{Name: ActionExceptionDelete, Group: "Logs and audit", Summary: "Delete an exception group and its occurrences; a later occurrence starts a new group.", Params: []Param{pApp, req("exp_uid", TypeString, "exception fingerprint")}},

	{Name: ActionEvents, Group: "Events", Summary: "Event summary: counts per event, namespace and day.", Params: []Param{pApp, pQuery}},
	{Name: ActionEventsLatest, Group: "Events", Summary: "Newest matching events.", Params: []Param{pApp, pQuery, opt("lines", TypeInteger, "row limit; default 50")}},
	{Name: ActionEventsFacets, Group: "Events", Summary: "Value counts of one key across the matching events.", Params: []Param{pApp, pQuery, opt("key", TypeString, "facet key: a column, `#` for tags or `data.<field>`")}},
	{Name: ActionEventsViews, Group: "Events", Summary: "Saved views and funnels, from dboss.yaml and the console.", Params: []Param{pApp}},
	{Name: ActionEventsFunnel, Group: "Events", Summary: "Run a saved funnel by name, or the funnel definition in data. Needs the duckdb CLI.", Params: []Param{
		pApp,
		opt("name", TypeString, "saved funnel name"),
		opt("data", TypeJSON, "funnel definition, used instead of name"),
		opt("query", TypeString, "filter that narrows step 1"),
	}},
	{Name: ActionEventsQuery, Group: "Events", Summary: "Run SQL against the app's events in a sandboxed DuckDB (30s, 500 rows). Needs the duckdb CLI.", Params: []Param{pApp, req("sql", TypeString, "DuckDB SQL over the events views")}},
	{Name: ActionEventsSave, Group: "Events", Summary: "Save a view or funnel on the console side.", Params: []Param{pApp, req("kind", TypeString, "view or funnel"), req("data", TypeJSON, "the view or funnel, with its name")}},
	{Name: ActionEventsDelete, Group: "Events", Summary: "Delete a console-saved view or funnel.", Params: []Param{pApp, req("kind", TypeString, "view or funnel"), req("name", TypeString, "its name")}},

	{Name: ActionPG, Group: "PostgreSQL", Summary: "Fresh inspection of the PostgreSQL server: databases, sizes and backup settings."},
	{Name: ActionPGBackups, Group: "PostgreSQL", Summary: "The backup catalog, newest first."},
	{Name: ActionPGBackup, Group: "PostgreSQL", Summary: "Dump one database now (kept until deleted), or every selected database when database is empty.", Params: []Param{opt("database", TypeString, "database name")}},
	{Name: ActionPGRestore, Group: "PostgreSQL", Summary: "Restore a backup into a new database, or over an existing one with replace and confirm.", Params: []Param{
		req("backup_id", TypeString, "backup id from pg-backups"),
		opt("target", TypeString, "target database; defaults to the dumped one"),
		opt("replace", TypeBoolean, "drop and recreate an existing target"),
		opt("confirm", TypeString, "the target name, required with replace"),
	}},
	{Name: ActionPGDeleteDump, Group: "PostgreSQL", Summary: "Delete one backup file and its catalog row.", Params: []Param{req("backup_id", TypeString, "backup id from pg-backups")}},
	{Name: ActionPGDrop, Group: "PostgreSQL", Summary: "Drop a database. postgres and the templates are refused.", Params: []Param{req("database", TypeString, "database name"), req("confirm", TypeString, "the database name again")}},
	{Name: ActionPGQuery, Group: "PostgreSQL", Summary: "Run SQL on one database (30s timeout, 500 rows); a batch runs statement by statement and returns the last result.", Params: []Param{req("database", TypeString, "database name"), req("sql", TypeString, "SQL to run")}},

	{Name: ActionPubsub, Group: "PubSub", Summary: "Every pubsub hub with its channels and subscriber counts."},
	{Name: ActionPubsubSecret, Group: "PubSub", Summary: "The publish secret of one hub.", Params: []Param{pApp, opt("process", TypeString, "web process; needed when the app has several hubs")}},
	{Name: ActionPubsubRotate, Group: "PubSub", Summary: "Replace a hub's generated publish secret.", Params: []Param{pApp, opt("process", TypeString, "web process; needed when the app has several hubs")}},
	{Name: ActionPubsubPublish, Group: "PubSub", Summary: "Publish one message to a channel; returns how many subscribers got it.", Params: []Param{
		pApp,
		opt("process", TypeString, "web process; needed when the app has several hubs"),
		req("channel", TypeString, "channel name"),
		opt("event", TypeString, "event name"),
		opt("data", TypeJSON, "message payload"),
	}},
}

func init() {
	for i := range specs {
		specs[i].Audited = auditActions[specs[i].Name]
	}
}

// Specs lists every action with its parameters.
func Specs() []Spec { return specs }

// SpecFor returns one action's spec.
func SpecFor(name string) (Spec, bool) {
	for _, spec := range specs {
		if spec.Name == name {
			return spec, true
		}
	}
	return Spec{}, false
}
