package bench

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSettleRequiresTwoCleanScrapesOfExportedGauges(t *testing.T) {
	defer func(limit, poll time.Duration) { settleLimit, settlePoll = limit, poll }(settleLimit, settlePoll)
	settleLimit, settlePoll = 2*time.Second, time.Millisecond
	drainedBody := "pitot_ingest_queue_depth{signal=\"spans\"} 0\npitot_rollup_backlog_chunks{rollup=\"service\"} 0\npitot_read_cache_pending_batches 0\n"
	for _, tc := range []struct {
		name      string
		bodies    []string
		unsettled bool
		scrapes   int64
	}{
		{"drains after backlog", []string{"pitot_ingest_queue_depth{signal=\"spans\"} 3\npitot_rollup_backlog_chunks{rollup=\"service\"} 0\n", drainedBody, "pitot_rollup_backlog_chunks{rollup=\"service\"} 1\npitot_ingest_queue_depth{signal=\"spans\"} 0\n", drainedBody, drainedBody}, false, 5},
		{"read cache lags", []string{"pitot_ingest_queue_depth{signal=\"spans\"} 0\npitot_rollup_backlog_chunks{rollup=\"service\"} 0\npitot_read_cache_pending_batches 7\n"}, true, 0},
		{"absent gauge never drains", []string{"pitot_ingest_queue_depth{signal=\"spans\"} 0\n"}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				i := int(n.Add(1)) - 1
				if i >= len(tc.bodies) {
					i = len(tc.bodies) - 1
				}
				_, _ = w.Write([]byte(tc.bodies[i]))
			}))
			defer srv.Close()
			r := &Runner{Config: Config{MetricsURL: srv.URL}, client: srv.Client()}
			_, unsettled := r.settle(context.Background(), settleLimit)
			if unsettled != tc.unsettled {
				t.Fatalf("unsettled=%v", unsettled)
			}
			if tc.scrapes > 0 && n.Load() != tc.scrapes {
				t.Fatalf("scrapes=%d want %d", n.Load(), tc.scrapes)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &Runner{Config: Config{MetricsURL: "http://127.0.0.1:1"}, client: http.DefaultClient}
	if _, unsettled := r.settle(ctx, settleLimit); !unsettled {
		t.Fatal("canceled settle reported settled")
	}
}
