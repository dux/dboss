package cloudflare

import "fmt"

type Action string

const (
	ActionOK       Action = "ok"
	ActionCreate   Action = "create"
	ActionUpdate   Action = "update"
	ActionConflict Action = "conflict"
)

// Change is what it takes to put one wanted record in place. Current is the record it updates or
// matches, or the one in the way of a conflict; Reason says why a conflict is one.
type Change struct {
	Want    Record
	Current *Record
	Action  Action
	Reason  string
}

// Plan decides how want lands at its name, given every record there now. Only a record of the
// wanted type and a CNAME stand in the way; any other type (MX, TXT, the other address family)
// shares the name and is left alone. Records are only ever created or updated in place, never
// deleted, so a second record of the wanted type is a conflict the operator resolves, and a CNAME
// is only rewritten with force.
func Plan(want Record, existing []Record, force bool) Change {
	var same, cname []Record
	for _, record := range existing {
		switch record.Type {
		case want.Type:
			same = append(same, record)
		case "CNAME":
			cname = append(cname, record)
		}
	}
	switch {
	case len(same) > 1:
		return Change{Want: want, Current: &same[0], Action: ActionConflict, Reason: fmt.Sprintf("%d %s records; remove all but one in Cloudflare", len(same), want.Type)}
	case len(same) == 1:
		current := same[0]
		want.ID = current.ID
		if current.Content == want.Content && current.Proxied == want.Proxied {
			return Change{Want: want, Current: &current, Action: ActionOK}
		}
		return Change{Want: want, Current: &current, Action: ActionUpdate}
	case len(cname) > 0 && !force:
		return Change{Want: want, Current: &cname[0], Action: ActionConflict, Reason: "a CNAME is there; --force replaces it"}
	case len(cname) > 0:
		current := cname[0]
		want.ID = current.ID
		return Change{Want: want, Current: &current, Action: ActionUpdate}
	}
	return Change{Want: want, Action: ActionCreate}
}
