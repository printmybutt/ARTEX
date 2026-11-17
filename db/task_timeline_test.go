package db

import (
	"strconv"
	"testing"
	"time"
)

// deref unwraps a *int64 token field, failing the test when a bucket that has
// ledger rows unexpectedly carries a nil token value.
func deref(t *testing.T, label string, p *int64) int64 {
	t.Helper()
	if p == nil {
		t.Fatalf("%s is nil but bucket has usage", label)
	}
	return *p
}

func TestTaskTimelineAggregatesLedgerAndActivity(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}

	task, err := d.CreateTask("timeline-usage", "timeline goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	from := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	to := from.Add(2 * time.Hour)
	if _, err := d.Exec(`
INSERT INTO llm_usage(task_id, exploration_id, worker, model, profile_name, input_tokens, output_tokens, cache_read, cache_write, status, ts)
VALUES
 ($1,$2,'planner','model-a','p',100,20,40,5,'ok',$3),
 ($1,$2,'worker','model-a','p',200,30,60,10,'error',$4),
 ($1,$2,'worker','model-b','p',400,50,80,20,'ok',$5)`,
		strconv.FormatInt(task.ID, 10), task.ExplorationID, from.Add(30*time.Minute), from.Add(90*time.Minute), to.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`
INSERT INTO activity(exploration_id, worker, kind, created_at)
VALUES
 ($1,'planner','round',$2),
 ($1,'planner','round',$3),
 ($1,'worker','result',$3),
 ($1,'worker','tool_use',$3)`,
		task.ExplorationID, from.Add(31*time.Minute), from.Add(91*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`
UPDATE activity SET input_tokens=11, output_tokens=7, cache_read_tokens=3, cache_write_tokens=2
WHERE exploration_id=$1 AND kind='result'`, task.ExplorationID); err != nil {
		t.Fatal(err)
	}

	result, err := d.TaskTimeline(task.ID, task.ExplorationID, from, to, 900, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.UsageStatus != "recorded" {
		t.Fatalf("usage status=%q", result.UsageStatus)
	}
	var first, second TaskTimelineBucket
	for _, bucket := range result.Buckets {
		if bucket.Start.Equal(from.Add(30 * time.Minute)) {
			first = bucket
		}
		if bucket.Start.Equal(from.Add(90 * time.Minute)) {
			second = bucket
		}
	}
	if !first.HasUsage || first.Calls != 1 || first.FailedCalls != 0 || deref(t, "first.InputTokens", first.InputTokens) != 100 ||
		deref(t, "first.OutputTokens", first.OutputTokens) != 20 || deref(t, "first.CacheReadTokens", first.CacheReadTokens) != 40 ||
		deref(t, "first.CacheWriteTokens", first.CacheWriteTokens) != 5 ||
		first.PlannerRounds != 1 || first.WorkerResultRows != 0 {
		t.Fatalf("unexpected first bucket: %+v", first)
	}
	if !second.HasUsage || second.Calls != 1 || second.FailedCalls != 1 || deref(t, "second.InputTokens", second.InputTokens) != 200 ||
		deref(t, "second.OutputTokens", second.OutputTokens) != 30 || deref(t, "second.CacheReadTokens", second.CacheReadTokens) != 60 ||
		deref(t, "second.CacheWriteTokens", second.CacheWriteTokens) != 10 ||
		second.PlannerRounds != 1 || second.WorkerResultRows != 1 {
		t.Fatalf("unexpected second bucket: %+v", second)
	}
	if result.SessionTotals.InputTokens != 11 || result.SessionTotals.OutputTokens != 7 ||
		result.SessionTotals.CacheReadTokens != 3 || result.SessionTotals.CacheWriteTokens != 2 {
		t.Fatalf("unexpected session totals: %+v", result.SessionTotals)
	}
	if len(result.EventMarkers) != 3 || result.HasMoreEvents {
		t.Fatalf("unexpected event markers: %+v more=%v", result.EventMarkers, result.HasMoreEvents)
	}
}

func TestTaskTimelineNoLedgerRows(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}

	task, err := d.CreateTask("timeline-empty", "timeline goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	from := time.Now().UTC().Truncate(time.Hour)
	result, err := d.TaskTimeline(task.ID, task.ExplorationID, from, from.Add(time.Hour), 900, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.UsageStatus != "no_rows" || len(result.Buckets) != 4 {
		t.Fatalf("unexpected empty result: status=%q buckets=%d", result.UsageStatus, len(result.Buckets))
	}
	for _, bucket := range result.Buckets {
		if bucket.HasUsage || bucket.InputTokens != nil || bucket.OutputTokens != nil ||
			bucket.CacheReadTokens != nil || bucket.CacheWriteTokens != nil {
			t.Fatalf("empty bucket should not be marked as usage: %+v", bucket)
		}
	}
}

func TestTaskTimelineNegativeRangeReturnsEmpty(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}

	task, err := d.CreateTask("timeline-negative", "timeline goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	from := time.Now().UTC()
	result, err := d.TaskTimeline(task.ID, task.ExplorationID, from, from.Add(-time.Hour), 900, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.UsageStatus != "no_rows" || len(result.Buckets) != 0 || len(result.EventMarkers) != 0 {
		t.Fatalf("negative range should return an empty result, got %+v", result)
	}
}

func TestTaskTimelinePartialBucketRoundsUp(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}

	task, err := d.CreateTask("timeline-partial", "timeline goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	// 1.5 hours at a 15-minute grain is 6 buckets; the trailing half hour must
	// not add a phantom 7th bucket.
	from := time.Now().UTC().Add(-90 * time.Minute).Truncate(15 * time.Minute)
	result, err := d.TaskTimeline(task.ID, task.ExplorationID, from, from.Add(90*time.Minute), 900, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Buckets) != 6 {
		t.Fatalf("expected 6 buckets, got %d", len(result.Buckets))
	}
}
