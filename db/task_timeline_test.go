package db

import (
	"testing"
	"time"
)

func TestTaskTimelineAggregatesLedgerAndActivity(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })

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
		task.ID, task.ExplorationID, from.Add(30*time.Minute), from.Add(90*time.Minute), to.Add(time.Minute)); err != nil {
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
	if !first.HasUsage || first.Calls != 1 || first.FailedCalls != 0 || first.InputTokens != 100 ||
		first.OutputTokens != 20 || first.CacheReadTokens != 40 || first.CacheWriteTokens != 5 ||
		first.PlannerRounds != 1 || first.WorkerResultRows != 0 {
		t.Fatalf("unexpected first bucket: %+v", first)
	}
	if !second.HasUsage || second.Calls != 1 || second.FailedCalls != 1 || second.InputTokens != 200 ||
		second.OutputTokens != 30 || second.CacheReadTokens != 60 || second.CacheWriteTokens != 10 ||
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
		if bucket.HasUsage || bucket.InputTokens != 0 {
			t.Fatalf("empty bucket should not be marked as usage: %+v", bucket)
		}
	}
}
