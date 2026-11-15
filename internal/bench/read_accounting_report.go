package bench

import (
	"fmt"
	"io"
	"sort"
)

func renderReadEvidence(w io.Writer, r Report) {
	if r.Settings.ReadProfile == nil {
		fmt.Fprintln(w, "\nLegacy read evidence has no scheduled-arrival qualification profile. Its original service-time metrics and recorded gates are historical; they do not establish qualification against the current goals.")
		return
	}
	p := r.Settings.ReadProfile
	fmt.Fprintf(w, "\n### HTTP operation completion goals\n\nProfile `%s` declares proposed engineering goals, not measured product guarantees. HTTP operation completion is separate from browser last-panel paint. Qualification requires at least %d successful samples per performed kind. Short sweeps report insufficient evidence and use the documented topology fallback.\n\n", p.Name, p.MinSamples)
	fmt.Fprintln(w, "| HTTP operation | p95 goal ms | p99 goal ms |\n|---|---:|---:|")
	for _, kind := range sortedReadKeys(p.Targets) {
		target := p.Targets[kind]
		fmt.Fprintf(w, "| %s | %.0f | %.0f |\n", kind, target.P95Ms, target.P99Ms)
	}
	for _, t := range r.Trials {
		if t.Spec.QueryRate <= 0 || t.Read == nil {
			continue
		}
		e := t.Read
		fmt.Fprintf(w, "\n### Arrival accounting: %s\n\nScheduled: %d over %.3fs; scheduler-missed: %d; completed during load: %d (%.2f/s); completed during drain: %d. Service-time fields above exclude the scheduler queue; successful arrival-to-completion fields below include it. Failures, cancellations and shed slots never enter successful-latency percentiles.\n\n", t.Spec.Name, e.Expected, e.ScheduleSeconds, e.SchedulerMissed, e.CompletedDuringLoad, readCompletedRate(t), e.CompletedDuringDrain)
		fmt.Fprintln(w, "| Intended operation | Scheduled | Scheduler missed | Shed | Dispatched | Successful | Failures | Canceled | Performed as another kind |\n|---|---:|---:|---:|---:|---:|---:|---:|---:|")
		for _, kind := range sortedReadKeys(e.Intended) {
			s := e.Intended[kind]
			fmt.Fprintf(w, "| %s | %d | %d | %d | %d | %d | %d | %d | %d |\n", kind, s.Scheduled, s.SchedulerMissed, s.Shed, s.Dispatched, s.Successful, s.Failures, s.Canceled, s.Fallbacks)
		}
		fmt.Fprintln(w, "\n| Performed HTTP operation | Dispatched | Successful | Failures | Canceled | Queue p95 ms | Arrival-to-completion p95 ms | Arrival-to-completion p99 ms | Evidence |\n|---|---:|---:|---:|---:|---:|---:|---:|---|")
		for _, kind := range sortedReadKeys(e.Operations) {
			s := e.Operations[kind]
			evidence := "sufficient samples; see qualification gate"
			if s.EndToEnd.Count < p.MinSamples {
				evidence = "insufficient successful samples"
			}
			fmt.Fprintf(w, "| %s | %d | %d | %d | %d | %.2f | %.2f | %.2f | %s |\n", kind, s.Dispatched, s.EndToEnd.Count, s.Failures, s.Canceled, s.Queue.P95Ms, s.EndToEnd.P95Ms, s.EndToEnd.P99Ms, evidence)
		}
	}
}

func sortedReadKeys[V any](items map[string]V) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
