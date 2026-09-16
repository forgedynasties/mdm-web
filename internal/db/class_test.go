package db

import (
	"strings"
	"testing"

	prod "mdm/internal/product"
)

// The composition strip on the fleet page derives a device's category in SQL while
// the filters derive it in Go. They used to be written out separately and drifted;
// both now come from the catalog, so this pins them together.
func TestDerivedClassSQLMatchesCatalog(t *testing.T) {
	sql := derivedClassSQL()
	for _, p := range prod.All() {
		if p.Class == "" {
			continue
		}
		if !strings.Contains(sql, "'"+p.Key+"'") {
			t.Errorf("derivedClassSQL() is missing product %q: %s", p.Key, sql)
		}
		if !strings.Contains(sql, "THEN '"+p.Class+"'") {
			t.Errorf("derivedClassSQL() is missing class %q: %s", p.Class, sql)
		}
	}
	// The legacy empty product is the default product, so it must ride along with that
	// product's category — otherwise the 21 pre-product devices fall out of their bucket.
	if !strings.Contains(sql, "''") {
		t.Errorf("derivedClassSQL() drops the legacy empty product: %s", sql)
	}
}

func TestProductKeysForClass(t *testing.T) {
	t7 := productKeysForClass(prod.ClassT7)
	var hasT7, hasEmpty bool
	for _, k := range t7 {
		switch k {
		case prod.KeyT7:
			hasT7 = true
		case "":
			hasEmpty = true
		}
	}
	if !hasT7 || !hasEmpty {
		t.Errorf("productKeysForClass(t7) = %q, want both t7 and the legacy empty key", t7)
	}
	kiosks := productKeysForClass(prod.ClassKiosk)
	if len(kiosks) != 3 {
		t.Errorf("productKeysForClass(kiosk) = %q, want the three kiosk products", kiosks)
	}
	if got := productKeysForClass(prod.ClassMPOS); len(got) != 0 {
		t.Errorf("productKeysForClass(mpos) = %q, want none (no catalog product is an mPOS)", got)
	}
}
