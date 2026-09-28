package db

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"mdm/internal/metrics"
)

// queryTracer adds each query's time to the request it ran under, for the Server
// page's per-route DB column. Outside a request (tickers, the ingest queue) there is
// no counter in the context and it does nothing beyond a context lookup.
//
// For a query that returns rows, pgx ends the trace when the rows are closed, so the
// time includes the handler's scan loop — "time holding a query open", which is the
// number that matters for pool pressure anyway.
type queryTracer struct{}

type traceStartKey struct{}

func (queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if metrics.DBUsageFrom(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, traceStartKey{}, time.Now())
}

func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	u := metrics.DBUsageFrom(ctx)
	if u == nil {
		return
	}
	if t, ok := ctx.Value(traceStartKey{}).(time.Time); ok {
		u.Add(time.Since(t))
	}
}
