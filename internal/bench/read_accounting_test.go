package bench

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestReadScheduleCountsMissedArrivalsWithoutCatchupBurst(t *testing.T) {
	q := newQueryRun()
	q.targets = []queryTarget{{name: "dashboard-refresh"}, {name: "drill-traces"}, {name: "trace-detail"}}
	q.startedAt = time.Unix(0, 0)
	q.rate = 20
	var jobs []readJob
	q.advanceSchedule(q.startedAt.Add(250*time.Millisecond), func(job readJob) bool {
		jobs = append(jobs, job)
		return true
	})
	q.advanceSchedule(q.startedAt.Add(500*time.Millisecond), func(job readJob) bool {
		jobs = append(jobs, job)
		return false
	})
	evidence := q.readEvidence()
	if len(jobs) != 2 || jobs[0].slot != 5 || jobs[1].slot != 10 || !jobs[0].scheduledAt.Equal(q.startedAt.Add(250*time.Millisecond)) {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
	if evidence.Expected != 10 || evidence.SchedulerMissed != 8 || q.shed != 1 || q.attempts != 2 {
		t.Fatalf("arrivals disappeared or burst: %+v attempts=%d shed=%d", evidence, q.attempts, q.shed)
	}
	var offered, missed, shed, dispatched int64
	for _, kind := range evidence.Intended {
		offered += kind.Scheduled
		missed += kind.SchedulerMissed
		shed += kind.Shed
		dispatched += kind.Dispatched
	}
	if offered != 10 || missed != 8 || shed != 1 || dispatched != 1 {
		t.Fatalf("intended accounting: %+v", evidence.Intended)
	}
}

func TestReadOperationSeparatesQueueTimeAndDrainFromServiceLatency(t *testing.T) {
	q := newQueryRun()
	at := time.Unix(0, 0)
	q.cutoff = at.Add(500 * time.Millisecond)
	q.recordOperation(readJob{scheduledAt: at, intended: "trace-detail"}, "dashboard-refresh", at.Add(time.Second), at.Add(time.Second+2*time.Millisecond), true, false)
	evidence := q.readEvidence()
	if service := q.hist.snapshot(); service.Count != 1 || service.MaxMs != 2 {
		t.Fatalf("legacy service latency changed: %+v", service)
	}
	if evidence.EndToEnd.MaxMs != 1002 || evidence.Queue.MaxMs != 1000 || evidence.CompletedDuringLoad != 0 || evidence.CompletedDuringDrain != 1 {
		t.Fatalf("queue or drain hidden: %+v", evidence)
	}
	if evidence.Intended["trace-detail"].Fallbacks != 1 || evidence.Operations["dashboard-refresh"].EndToEnd.Count != 1 {
		t.Fatalf("fallback hid intended operation: %+v", evidence)
	}
}

func TestReadScheduleHandlesFractionalRatesAndFinalMissedSlots(t *testing.T) {
	q := newQueryRun()
	q.startedAt, q.rate = time.Unix(0, 0), 2.5
	dispatched := 0
	dispatch := func(readJob) bool { dispatched++; return true }
	q.advanceSchedule(q.startedAt.Add(400*time.Millisecond-time.Nanosecond), dispatch)
	q.advanceSchedule(q.startedAt.Add(400*time.Millisecond), dispatch)
	q.advanceSchedule(q.startedAt.Add(400*time.Millisecond), dispatch)
	q.scheduleMu.Lock()
	q.cutoff = q.startedAt.Add(1200 * time.Millisecond)
	q.advanceScheduleLocked(q.cutoff, nil)
	q.scheduleMu.Unlock()
	q.advanceSchedule(q.startedAt.Add(10*time.Second), dispatch)
	e := q.readEvidence()
	if dispatched != 1 || e.Expected != 3 || e.SchedulerMissed != 2 || e.ScheduleSeconds != 1.2 {
		t.Fatalf("fractional boundaries/finalization lost arrivals: %+v dispatched=%d", e, dispatched)
	}
}

func TestReadOutcomesAccountForFailedCanceledAndFallbackIntents(t *testing.T) {
	q := newQueryRun()
	q.targets = []queryTarget{{name: "dashboard-refresh"}, {name: "drill-traces"}, {name: "trace-detail"}}
	q.startedAt, q.rate = time.Unix(0, 0), 20
	var jobs []readJob
	for slot := 1; slot <= 3; slot++ {
		q.advanceSchedule(q.startedAt.Add(time.Duration(slot)*50*time.Millisecond), func(job readJob) bool {
			jobs = append(jobs, job)
			return true
		})
	}
	for i, job := range jobs {
		kind := job.intended
		if i == 1 {
			kind = "dashboard-refresh"
		}
		q.recordOperation(job, kind, job.scheduledAt, job.scheduledAt.Add(time.Millisecond), i == 0, i == 2)
	}
	e := q.readEvidence()
	if e.Expected != 3 || q.hist.snapshot().Count != 1 || q.failures != 1 || q.canceled != 1 || e.Intended["drill-traces"].Fallbacks != 1 || e.Intended["drill-traces"].Failures != 1 || e.Intended["trace-detail"].Canceled != 1 {
		t.Fatalf("failed/canceled/fallback work disappeared: %+v", e)
	}
	if e.Operations["dashboard-refresh"].Dispatched != 2 || e.Operations["trace-detail"].Canceled != 1 {
		t.Fatalf("performed outcomes lost: %+v", e.Operations)
	}
}

func qualifiedReadTrial() Trial {
	profile := currentReadProfile()
	read := &ReadEvidence{Profile: profile.Name, ScheduleSeconds: 150, Expected: 3000, CompletedDuringLoad: 3000, EndToEnd: Latency{Count: 3000}, Operations: map[string]ReadOperation{}, Intended: map[string]ReadIntended{}}
	for kind := range profile.Targets {
		read.Operations[kind] = ReadOperation{Dispatched: 1000, EndToEnd: Latency{Count: 1000, P95Ms: 1, P99Ms: 2}}
		read.Intended[kind] = ReadIntended{Scheduled: 1000, Dispatched: 1000, Successful: 1000}
	}
	return Trial{Spec: PhaseSpec{Signal: Mixed, Kind: "sustained", QueryRate: 20}, ElapsedSeconds: 150, QueryAttempts: 3000, QueryLatency: Latency{Count: 3000, P95Ms: 1, P99Ms: 2}, QueryKindCompleted: map[string]int64{"dashboard-refresh": 1000, "drill-traces": 1000, "trace-detail": 1000}, Read: read}
}

func TestReadGateRejectsSlowKindInsufficientSamplesAndLegacyEvidence(t *testing.T) {
	trial := qualifiedReadTrial()
	if failures := readGateFailures(trial); len(failures) != 0 {
		t.Fatalf("valid qualification failed: %v", failures)
	}
	operation := trial.Read.Operations["drill-traces"]
	operation.EndToEnd.P99Ms = 1501
	trial.Read.Operations["drill-traces"] = operation
	if queryTargetMet(trial) || !strings.Contains(strings.Join(readGateFailures(trial), " "), "drill-traces") {
		t.Fatal("fast aggregate hid slow drill-down")
	}
	trial = qualifiedReadTrial()
	operation = trial.Read.Operations["trace-detail"]
	operation.EndToEnd.Count = 999
	trial.Read.Operations["trace-detail"] = operation
	if !strings.Contains(strings.Join(readGateFailures(trial), " "), "insufficient") {
		t.Fatal("p99 qualified with insufficient samples")
	}
	trial.Read = nil
	if queryTargetMet(trial) {
		t.Fatal("legacy service latency qualified as new arrival latency")
	}
}

func TestReadEvidenceRoundtripPreservesLegacyMetricMeaningAndTargetIdentity(t *testing.T) {
	report := fixtureReport()
	report.Settings.QueryRate = 20
	report.Settings.ReadProfile = currentReadProfile()
	report.Trials = []Trial{qualifiedReadTrial()}
	before := workloadHash(report.Settings, nil)
	report.Trials[0].Read.EndToEnd.P95Ms = 42
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var restored Report
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Trials[0].QueryLatency.P95Ms != 1 || restored.Trials[0].Read.EndToEnd.P95Ms != 42 || before != workloadHash(restored.Settings, nil) {
		t.Fatal("metric meaning or workload identity changed during roundtrip")
	}
	profile := *restored.Settings.ReadProfile
	profile.Name = "different-profile"
	restored.Settings.ReadProfile = &profile
	if workloadHash(restored.Settings, nil) == before {
		t.Fatal("declared targets absent from workload identity")
	}
	markdown := Markdown(restored)
	if !strings.Contains(markdown, "HTTP operation") || !strings.Contains(markdown, "Scheduled") || !strings.Contains(markdown, "1500") {
		t.Fatalf("new accounting/profile missing from report: %s", markdown)
	}
}

func TestReadProfileAndPerKindLatencyControlQualificationAndTopology(t *testing.T) {
	fast := qualifiedReadTrial()
	fast.Spec.Kind, fast.Spec.ProcessesPerSignal = "sweep", 4
	fast.Accepted, fast.AcceptedPerSecond = Counts{Spans: 1}, 150
	slow := qualifiedReadTrial()
	slow.Spec.Kind, slow.Spec.ProcessesPerSignal = "sweep", 8
	slow.Accepted, slow.AcceptedPerSecond = Counts{Spans: 1}, 300
	operation := slow.Read.Operations["trace-detail"]
	operation.EndToEnd.P95Ms = 301
	slow.Read.Operations["trace-detail"] = operation
	if got := bestMixedTopology([]Trial{slow, fast}); got != 4 {
		t.Fatalf("slow HTTP class selected by aggregate throughput: %d", got)
	}
	trial := qualifiedReadTrial()
	trial.Accepted = Counts{Spans: 1}
	for _, profile := range []*ReadProfile{nil, currentReadProfile()} {
		if profile != nil {
			profile.Targets["dashboard-refresh"] = ReadTarget{P95Ms: 9999, P99Ms: 9999}
		}
		summary := summarize(Report{Settings: Settings{QueryRate: 20, ReadProfile: profile}, Trials: []Trial{trial}}, 0, 0)
		if summary.Passed {
			t.Fatal("missing or silently weakened declared profile qualified")
		}
	}
}
