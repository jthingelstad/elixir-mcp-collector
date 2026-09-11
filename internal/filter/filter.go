// Package filter applies what a lease asks the collector to drop before
// submitting. The hub owns the decision (it hands the mark in the lease);
// this is the mechanical part, kept apart so both twins pin it the same
// way. The body stays the API's own array - fewer entries, same shape.
package filter

import (
	"bytes"
	"encoding/json"
)

// Filter is the lease's `filter` object.
type Filter struct {
	// The newest battleTime the hub already holds, in the API's own
	// spelling (20260911T123456.000Z): battleTime strings compare
	// lexically, so nothing here parses a date.
	BattlesAfter string `json:"battles_after"`
}

// Result of applying a filter to a battlelog body.
type Result struct {
	Body     string // the array with the dropped entries removed
	Observed int    // entries before the filter
	Filtered int    // entries dropped
	Applied  bool   // false when the body was not an array to filter
}

// Battlelog keeps the entries whose battleTime is after the mark. A body
// that is not a JSON array (an error object, a shape we do not know) is
// returned untouched with Applied=false, so the hub still sees exactly
// what the API said.
func Battlelog(body string, after string) Result {
	if after == "" {
		return Result{Body: body}
	}
	var entries []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	if err := dec.Decode(&entries); err != nil {
		return Result{Body: body}
	}
	kept := make([]json.RawMessage, 0, len(entries))
	for _, e := range entries {
		var probe struct {
			BattleTime string `json:"battleTime"`
		}
		// An entry without a readable battleTime is kept: dropping what
		// we cannot judge would be a silent loss.
		if err := json.Unmarshal(e, &probe); err != nil || probe.BattleTime == "" || probe.BattleTime > after {
			kept = append(kept, e)
		}
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return Result{Body: body}
	}
	return Result{
		Body:     string(out),
		Observed: len(entries),
		Filtered: len(entries) - len(kept),
		Applied:  true,
	}
}
