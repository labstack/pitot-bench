package bench

import (
	"fmt"
	"math"
	"sync/atomic"
	"time"
)

// The profile is a proposed engineering qualification goal, not a measured
// product guarantee. HTTP dashboard completion does not measure browser paint.
type ReadProfile struct {
	Name       string                `json:"name"`
	MinSamples int64                 `json:"minimum_successful_samples_per_kind"`
	Targets    map[string]ReadTarget `json:"http_operation_targets"`
}

type ReadTarget struct {
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
}

func currentReadProfile() *ReadProfile {
	return &ReadProfile{Name: "interactive-http-draft/v1", MinSamples: 1000, Targets: map[string]ReadTarget{
		"dashboard-refresh": {P95Ms: 1500, P99Ms: 3000},
		"drill-traces":      {P95Ms: 750, P99Ms: 1500},
		"trace-detail":      {P95Ms: 300, P99Ms: 750},
	}}
}

func readProfileForRate(rate float64) *ReadProfile {
	if rate <= 0 {
		return nil
	}
	return currentReadProfile()
}

func readProfileMatches(p *ReadProfile) bool {
	want := currentReadProfile()
	if p == nil || p.Name != want.Name || p.MinSamples != want.MinSamples || len(p.Targets) != len(want.Targets) {
		return false
	}
	for kind, target := range want.Targets {
		if p.Targets[kind] != target {
			return false
		}
	}
	return true
}

func readCompletedRate(t Trial) float64 {
	if t.Read != nil {
		return perSecond(float64(t.Read.CompletedDuringLoad), t.Read.ScheduleSeconds)
	}
	// Historical reports retain their historical rate, but cannot pass the new
	// qualification gate without arrival accounting and a declared profile.
	return perSecond(float64(t.QueryLatency.Count), t.ElapsedSeconds)
}

type ReadEvidence struct {
	Profile              string                   `json:"profile"`
	ScheduleSeconds      float64                  `json:"schedule_seconds"`
	Expected             int64                    `json:"scheduled"`
	SchedulerMissed      int64                    `json:"scheduler_missed"`
	CompletedDuringLoad  int64                    `json:"completed_during_load"`
	CompletedDuringDrain int64                    `json:"completed_during_drain"`
	EndToEnd             Latency                  `json:"successful_arrival_to_completion_ms"`
	Queue                Latency                  `json:"dispatched_queue_ms"`
	Operations           map[string]ReadOperation `json:"performed_operations"`
	Intended             map[string]ReadIntended  `json:"intended_operations"`
}

type ReadOperation struct {
	Dispatched int64   `json:"dispatched"`
	Failures   int64   `json:"failures"`
	Canceled   int64   `json:"canceled"`
	EndToEnd   Latency `json:"successful_arrival_to_completion_ms"`
	Queue      Latency `json:"dispatched_queue_ms"`
}

type ReadIntended struct {
	Scheduled       int64 `json:"scheduled"`
	SchedulerMissed int64 `json:"scheduler_missed"`
	Shed            int64 `json:"shed"`
	Dispatched      int64 `json:"dispatched"`
	Successful      int64 `json:"successful"`
	Failures        int64 `json:"failures"`
	Canceled        int64 `json:"canceled"`
	Fallbacks       int64 `json:"performed_as_another_kind"`
}

type readJob struct {
	slot        int64
	targetIndex int
	scheduledAt time.Time
	intended    string
}

// advanceSchedule accounts for every due arrival in bounded time, even when
// ticker deliveries are dropped. Missed slots are not replayed in a burst.
func (q *queryRun) advanceSchedule(now time.Time, dispatch func(readJob) bool) {
	q.scheduleMu.Lock()
	defer q.scheduleMu.Unlock()
	if !q.cutoff.IsZero() {
		return
	}
	q.advanceScheduleLocked(now, dispatch)
}

func (q *queryRun) advanceScheduleLocked(now time.Time, dispatch func(readJob) bool) {
	due := int64(math.Floor(float64(now.Sub(q.startedAt)) * q.rate / float64(time.Second)))
	if due <= q.expected {
		return
	}
	first := q.expected + 1
	q.expected = due
	lastMissed := due
	if dispatch != nil {
		lastMissed--
	}
	q.missed += lastMissed - first + 1
	q.attributeArrivals(first, lastMissed, "missed")
	if dispatch == nil {
		return
	}
	index := int((due - 1) % int64(len(q.targets)))
	job := readJob{slot: due, targetIndex: index, scheduledAt: q.startedAt.Add(time.Duration(float64(due) * float64(time.Second) / q.rate)), intended: q.targets[index].name}
	atomic.AddInt64(&q.attempts, 1)
	if dispatch(job) {
		q.attributeArrivals(due, due, "dispatched")
	} else {
		atomic.AddInt64(&q.shed, 1)
		q.attributeArrivals(due, due, "shed")
	}
}

// Attribute an arbitrary range of slots without looping over missed arrivals.
// Both target rotation and intended operation mix include shed/missed slots.
func (q *queryRun) attributeArrivals(first, last int64, outcome string) {
	if last < first {
		return
	}
	n := int64(len(q.targets))
	before := func(slots, index int64) int64 {
		count := slots / n
		if index < slots%n {
			count++
		}
		return count
	}
	for index, target := range q.targets {
		count := before(last, int64(index)) - before(first-1, int64(index))
		stats := q.intended[target.name]
		stats.Scheduled += count
		switch outcome {
		case "missed":
			stats.SchedulerMissed += count
		case "shed":
			stats.Shed += count
		case "dispatched":
			stats.Dispatched += count
		}
		q.intended[target.name] = stats
	}
}

func (q *queryRun) stopScheduling() {
	q.stopOnce.Do(func() {
		q.scheduleMu.Lock()
		// Capture the cutoff under the same lock used to record completions, so
		// completions cannot race a retroactively chosen measurement boundary.
		q.cutoff = time.Now()
		if !q.startedAt.IsZero() {
			q.advanceScheduleLocked(q.cutoff, nil)
		}
		q.scheduleMu.Unlock()
		close(q.stop)
	})
}

func (q *queryRun) recordOperation(job readJob, kind string, started, finished time.Time, success, canceled bool) {
	q.scheduleMu.Lock()
	defer q.scheduleMu.Unlock()
	q.kindsMu.Lock()
	defer q.kindsMu.Unlock()
	stats := q.kinds[kind]
	if stats == nil {
		stats = &operationStats{hist: newHistogram()}
		q.kinds[kind] = stats
	}
	if stats.endToEnd == nil {
		stats.endToEnd, stats.queue = newHistogram(), newHistogram()
	}
	stats.dispatched++
	queue := started.Sub(job.scheduledAt)
	stats.queue.record(queue)
	q.queue.record(queue)
	intended := q.intended[job.intended]
	if kind != job.intended {
		intended.Fallbacks++
	}
	if success {
		q.hist.record(finished.Sub(started))
		stats.hist.record(finished.Sub(started))
		q.endToEnd.record(finished.Sub(job.scheduledAt))
		stats.endToEnd.record(finished.Sub(job.scheduledAt))
		intended.Successful++
		if q.cutoff.IsZero() || !finished.After(q.cutoff) {
			q.completedDuringLoad++
		} else {
			q.completedDuringDrain++
		}
	} else if canceled {
		atomic.AddInt64(&q.canceled, 1)
		stats.canceled++
		intended.Canceled++
	} else {
		atomic.AddInt64(&q.failures, 1)
		stats.failures++
		intended.Failures++
	}
	q.intended[job.intended] = intended
}

func (q *queryRun) readEvidence() *ReadEvidence {
	q.scheduleMu.Lock()
	defer q.scheduleMu.Unlock()
	q.kindsMu.Lock()
	defer q.kindsMu.Unlock()
	e := &ReadEvidence{Profile: currentReadProfile().Name, Expected: q.expected, SchedulerMissed: q.missed, CompletedDuringLoad: q.completedDuringLoad, CompletedDuringDrain: q.completedDuringDrain, EndToEnd: q.endToEnd.snapshot(), Queue: q.queue.snapshot(), Operations: map[string]ReadOperation{}, Intended: map[string]ReadIntended{}}
	if !q.startedAt.IsZero() && !q.cutoff.IsZero() {
		e.ScheduleSeconds = q.cutoff.Sub(q.startedAt).Seconds()
	}
	for kind, stats := range q.kinds {
		if stats.endToEnd != nil {
			e.Operations[kind] = ReadOperation{Dispatched: stats.dispatched, Failures: stats.failures, Canceled: stats.canceled, EndToEnd: stats.endToEnd.snapshot(), Queue: stats.queue.snapshot()}
		}
	}
	for kind, stats := range q.intended {
		e.Intended[kind] = stats
	}
	return e
}

func readGateFailures(t Trial) []string {
	if t.Spec.QueryRate <= 0 {
		return nil
	}
	profile := currentReadProfile()
	e := t.Read
	if e == nil || e.Profile != profile.Name {
		return []string{"arrival accounting/profile missing; legacy service latency is not qualification evidence"}
	}
	var failures []string
	if e.ScheduleSeconds <= 0 || math.IsNaN(e.ScheduleSeconds) || math.IsInf(e.ScheduleSeconds, 0) || perSecond(float64(e.CompletedDuringLoad), e.ScheduleSeconds) < t.Spec.QueryRate*.95 {
		failures = append(failures, "during-load successful operation rate below target")
	}
	if e.Expected != t.QueryAttempts+e.SchedulerMissed || e.Expected <= 0 || e.SchedulerMissed != 0 || t.QueryShed != 0 || t.QueryFailures != 0 || t.QueryCanceled != 0 {
		failures = append(failures, "scheduled arrivals missing, shed, failed or canceled")
	}
	var scheduled, missed, shed, dispatched, successful, failed, canceled int64
	for _, stats := range e.Intended {
		scheduled += stats.Scheduled
		missed += stats.SchedulerMissed
		shed += stats.Shed
		dispatched += stats.Dispatched
		successful += stats.Successful
		failed += stats.Failures
		canceled += stats.Canceled
		if stats.Scheduled != stats.SchedulerMissed+stats.Shed+stats.Dispatched || stats.Dispatched != stats.Successful+stats.Failures+stats.Canceled {
			failures = append(failures, "intended operation accounting inconsistent")
		}
	}
	if scheduled != e.Expected || missed != e.SchedulerMissed || shed != t.QueryShed || dispatched != successful+failed+canceled || successful != t.QueryLatency.Count || successful != e.EndToEnd.Count || successful != e.CompletedDuringLoad+e.CompletedDuringDrain || failed != t.QueryFailures || canceled != t.QueryCanceled {
		failures = append(failures, "read outcome totals inconsistent")
	}
	var performed, performedSuccessful, performedFailed, performedCanceled int64
	for _, operation := range e.Operations {
		performed += operation.Dispatched
		performedSuccessful += operation.EndToEnd.Count
		performedFailed += operation.Failures
		performedCanceled += operation.Canceled
		if operation.Dispatched != operation.EndToEnd.Count+operation.Failures+operation.Canceled {
			failures = append(failures, "performed operation accounting inconsistent")
		}
	}
	if performed != dispatched || performedSuccessful != successful || performedFailed != failed || performedCanceled != canceled {
		failures = append(failures, "intended and performed outcome totals differ")
	}
	for _, kind := range []string{"dashboard-refresh", "drill-traces", "trace-detail"} {
		target := profile.Targets[kind]
		operation := e.Operations[kind]
		if e.Intended[kind].Scheduled == 0 || operation.EndToEnd.Count < profile.MinSamples {
			failures = append(failures, fmt.Sprintf("%s insufficient successful samples: %d/%d", kind, operation.EndToEnd.Count, profile.MinSamples))
			continue
		}
		if math.IsNaN(operation.EndToEnd.P95Ms) || math.IsNaN(operation.EndToEnd.P99Ms) || operation.EndToEnd.P95Ms > target.P95Ms || operation.EndToEnd.P99Ms > target.P99Ms {
			failures = append(failures, fmt.Sprintf("%s HTTP arrival-to-completion p95 %.1f/%.1fms, p99 %.1f/%.1fms", kind, operation.EndToEnd.P95Ms, target.P95Ms, operation.EndToEnd.P99Ms, target.P99Ms))
		}
	}
	return failures
}
