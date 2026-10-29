package dashboard

import (
	"context"
	"log"
	"os"
	"time"

	"mdm/internal/db"
)

// Monthly partitions for the telemetry tables (scalability plan W3; db/partitions.go).
// PARTITION_TELEMETRY=1 turns on the conversion and the partition-aware retention and
// thinning; without it housekeeping keeps its row-by-row paths, unchanged.
var partitionTelemetry = os.Getenv("PARTITION_TELEMETRY") == "1"

// unpartKeep is how long a converted table's old copy is kept, in case the swap needs
// undoing by hand.
const unpartKeep = 7 * 24 * time.Hour

// RunPartitions converts one table a run while any is left, keeps the coming months'
// partitions made, and drops old copies past unpartKeep. Hourly.
func (h *Handler) RunPartitions(ctx context.Context) {
	if !partitionTelemetry {
		return
	}
	for _, s := range db.PartSpecs {
		did, err := h.db.MigrateToPartitioned(ctx, s)
		if err != nil {
			log.Printf("[partitions] convert %s: %v", s.Table, err)
			return
		}
		if did {
			break // one conversion a run: each holds its table's writers for a moment
		}
	}
	if err := h.db.EnsurePartitions(ctx); err != nil {
		log.Printf("[partitions] ensure months: %v", err)
	}
	if err := h.db.DropUnpartitioned(ctx, unpartKeep); err != nil {
		log.Printf("[partitions] drop old copies: %v", err)
	}
}

// partitioned reports whether table is partitioned on this server and partitioning is on.
func (h *Handler) partitioned(ctx context.Context, table string) bool {
	if !partitionTelemetry {
		return false
	}
	ok, err := h.db.IsPartitioned(ctx, table)
	return err == nil && ok
}

// dropOldMonths is retention for a partitioned table: whole months older than keep go.
func (h *Handler) dropOldMonths(ctx context.Context, table string, keep time.Duration) {
	for _, s := range db.PartSpecs {
		if s.Table != table {
			continue
		}
		if n, err := h.db.DropMonthsBefore(ctx, s, time.Now().Add(-keep)); err != nil {
			log.Printf("[retention] drop %s months: %v", table, err)
		} else if n > 0 {
			log.Printf("[retention] dropped %d month(s) of %s older than %s", n, table, keep)
		}
	}
}
