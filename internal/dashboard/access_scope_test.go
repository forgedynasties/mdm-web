package dashboard

import (
	"reflect"
	"testing"

	"mdm/internal/db"
)

func TestKeepSerialsAndEnrollments(t *testing.T) {
	all := []string{"A1", "B2", "C3"}
	if got := keepSerials(all, nil); !reflect.DeepEqual(got, all) {
		t.Errorf("nil set must keep everything, got %v", got)
	}
	if got := keepSerials(all, map[string]bool{"B2": true}); !reflect.DeepEqual(got, []string{"B2"}) {
		t.Errorf("keepSerials = %v, want [B2]", got)
	}
	if got := keepSerials(all, map[string]bool{}); len(got) != 0 {
		t.Errorf("empty set (fail closed) must keep nothing, got %v", got)
	}
	rows := []db.Enrollment{{Serial: "A1"}, {Serial: "C3"}}
	if got := keepVisibleEnrollments(rows, map[string]bool{"C3": true}); len(got) != 1 || got[0].Serial != "C3" {
		t.Errorf("keepVisibleEnrollments = %+v", got)
	}
}
