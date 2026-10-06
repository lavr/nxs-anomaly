package engine

import (
	"strings"
	"testing"

	"github.com/nixys/nxs-anomaly/internal/utils"
)

// readiness_scoping_test.go holds the team-boundary checks, which both editions
// run: the boundary is part of the community edition too.

// Whether teams are a boundary is a deployment decision, and the readiness page
// is where an operator finds out which decision is in force. Neither mode is a
// defect; not having decided is.

func TestReadinessScopingOffIsStatedNotHidden(t *testing.T) {
	e, _ := readinessEngine(t)
	t.Setenv("NXS_ANOMALY_TEAM_SCOPING", "")

	c := check(t, mustReadiness(t, e), "team_scoping")
	if utils.StrVal(c, "severity") != ReadinessWarning {
		t.Errorf("severity = %q, want warning: a single trust domain is legitimate, but it should be seen", utils.StrVal(c, "severity"))
	}
	if !strings.Contains(utils.StrVal(c, "detail"), "every operator") {
		t.Errorf("detail does not say what scoping being off means: %q", utils.StrVal(c, "detail"))
	}
}

// The hole this exists for: scoping is on, so somebody drew a boundary, and an
// object with no team sits outside it while looking perfectly normal in the UI.
func TestReadinessScopingOnWithUnassignedObjectsBlocks(t *testing.T) {
	e, st := readinessEngine(t)
	t.Setenv("NXS_ANOMALY_TEAM_SCOPING", "true")
	st.seed("schedules", map[string]any{"id": "sch1", "name": "primary rotation"})

	report := mustReadiness(t, e)
	c := check(t, report, "team_scoping")
	if utils.StrVal(c, "severity") != ReadinessBlocker {
		t.Fatalf("severity = %q, want blocker; detail = %q", utils.StrVal(c, "severity"), utils.StrVal(c, "detail"))
	}
	// Named, not just counted: "two objects are unassigned" is not something an
	// operator can act on.
	var named []string
	for _, it := range anyList(c["items"]) {
		if str, ok := it.(string); ok {
			named = append(named, str)
		}
	}
	if !strings.Contains(strings.Join(named, " "), "primary rotation") {
		t.Errorf("items = %v, want the unassigned schedule named", named)
	}
	if report["ready"] == true {
		t.Error("ready = true with an object outside the boundary somebody turned on")
	}
}

func TestReadinessScopingOnWithEverythingAssignedPasses(t *testing.T) {
	e, st := readinessEngine(t)
	t.Setenv("NXS_ANOMALY_TEAM_SCOPING", "true")
	st.data["integrations"]["int1"]["team_id"] = "team1"
	st.seed("schedules", map[string]any{"id": "sch1", "name": "primary", "team_id": "team1"})

	c := check(t, mustReadiness(t, e), "team_scoping")
	if utils.StrVal(c, "severity") != ReadinessOK {
		t.Errorf("severity = %q, want ok; detail = %q", utils.StrVal(c, "severity"), utils.StrVal(c, "detail"))
	}
}
