package filter

import "testing"

const log = `[
 {"type":"PvP","battleTime":"20260911T130000.000Z","team":[{"tag":"#A"}]},
 {"type":"PvP","battleTime":"20260911T123456.000Z","team":[{"tag":"#A"}]},
 {"type":"PvP","battleTime":"20260911T120000.000Z","team":[{"tag":"#A"}]}
]`

func TestKeepsOnlyBattlesAfterTheMark(t *testing.T) {
	r := Battlelog(log, "20260911T123456.000Z")
	if !r.Applied || r.Observed != 3 || r.Filtered != 2 {
		t.Fatalf("%+v", r)
	}
	if r.Body != `[{"type":"PvP","battleTime":"20260911T130000.000Z","team":[{"tag":"#A"}]}]` {
		t.Fatalf("body: %s", r.Body)
	}
}

func TestNothingNewIsAnEmptyArrayWithTheCounts(t *testing.T) {
	r := Battlelog(log, "20260911T130000.000Z")
	if r.Body != `[]` || r.Observed != 3 || r.Filtered != 3 {
		t.Fatalf("%+v", r)
	}
}

func TestEverythingNewDropsNothing(t *testing.T) {
	r := Battlelog(log, "20260911T110000.000Z")
	if r.Filtered != 0 || r.Observed != 3 {
		t.Fatalf("%+v", r)
	}
}

func TestNoMarkOrNoArrayLeavesTheBodyAlone(t *testing.T) {
	if r := Battlelog(log, ""); r.Applied || r.Body != log {
		t.Fatalf("no mark: %+v", r)
	}
	errBody := `{"reason":"notFound"}`
	if r := Battlelog(errBody, "20260911T110000.000Z"); r.Applied || r.Body != errBody {
		t.Fatalf("not an array: %+v", r)
	}
}

func TestAnEntryWithoutBattleTimeIsKept(t *testing.T) {
	r := Battlelog(`[{"type":"odd"}]`, "20260911T110000.000Z")
	if r.Filtered != 0 || r.Body != `[{"type":"odd"}]` {
		t.Fatalf("%+v", r)
	}
}
