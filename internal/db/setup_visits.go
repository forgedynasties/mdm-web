package db

import (
	"github.com/google/uuid"
)

// Device states (roles replan 30 Oct 2026; simplified 19 Nov 2026: "we dont need this
// complexity"; no Lab state, 19 Nov 2026). A device is in one of two states, and the state
// decides which roles see it:
//
//   - To place    at no venue, waiting for one (placement.go)
//   - In service  at a venue
//
// Placing a device at a venue puts it in service at once. There is no Setting up state, no
// Hand over and no setup visit any more. The handed_over_* columns, the placement trigger
// and the setup_visits tables stay in the database (nothing destructive); the trigger still
// stamps handed_over_at when a device is placed, and the state ignores it.
const setupVisitsSchema = `
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'devices' AND column_name = 'handed_over_at') THEN
    ALTER TABLE devices ADD COLUMN handed_over_at TIMESTAMPTZ;
    ALTER TABLE devices ADD COLUMN handed_over_by TEXT NOT NULL DEFAULT '';
    -- Every device placed before device states existed is running at its venue.
    UPDATE devices SET handed_over_at = now(), handed_over_by = 'before setup states' WHERE restaurant_id IS NOT NULL;
  END IF;
END $$;
CREATE OR REPLACE FUNCTION devices_placement_reset() RETURNS trigger AS $$
BEGIN
  -- Placed at a venue = deployed (user, 3 Nov 2026: "when a device is placed at a venue its
  -- deployed. ops lead can manually put devices to set up bucket"); off a venue, no state.
  IF TG_OP = 'INSERT' THEN
    IF NEW.restaurant_id IS NOT NULL AND NEW.handed_over_at IS NULL THEN
      NEW.handed_over_at := now();
      NEW.handed_over_by := 'placed';
    END IF;
  ELSIF NEW.restaurant_id IS DISTINCT FROM OLD.restaurant_id THEN
    IF NEW.restaurant_id IS NULL THEN
      NEW.handed_over_at := NULL;
      NEW.handed_over_by := '';
    ELSE
      NEW.handed_over_at := now();
      NEW.handed_over_by := 'placed';
    END IF;
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'devices_placement_reset') THEN
    CREATE TRIGGER devices_placement_reset BEFORE UPDATE OF restaurant_id ON devices
      FOR EACH ROW EXECUTE FUNCTION devices_placement_reset();
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'devices_placement_insert') THEN
    CREATE TRIGGER devices_placement_insert BEFORE INSERT ON devices
      FOR EACH ROW EXECUTE FUNCTION devices_placement_reset();
  END IF;
END $$;
CREATE TABLE IF NOT EXISTS setup_visits (
	id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	restaurant_id UUID NOT NULL REFERENCES restaurants(id) ON DELETE CASCADE,
	purpose       TEXT NOT NULL DEFAULT '',
	ends_at       TIMESTAMPTZ NOT NULL,
	opened_by     TEXT NOT NULL DEFAULT '',
	opened_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
	closed_at     TIMESTAMPTZ,
	closed_by     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_setup_visits_restaurant ON setup_visits (restaurant_id, opened_at DESC);
CREATE TABLE IF NOT EXISTS setup_visit_people (
	visit_id UUID NOT NULL REFERENCES setup_visits(id) ON DELETE CASCADE,
	username TEXT NOT NULL,
	PRIMARY KEY (visit_id, username)
);
CREATE INDEX IF NOT EXISTS idx_setup_visit_people_user ON setup_visit_people (username);
`


// Device states.
const (
	StateToPlace   = "to_place" // at no venue, enrolled for a restaurant (placement.go)
	StateInService = "in_service"
)

// DeviceState is the device's state from its venue and placement.
func DeviceState(restaurantID *uuid.UUID, placement string) string {
	switch {
	case restaurantID != nil:
		return StateInService
	}
	// A device at no venue is waiting to be placed, whatever its placement says (no Lab state,
	// 19 Nov 2026: a legacy device that was never queued reads as To place too).
	return StateToPlace
}
