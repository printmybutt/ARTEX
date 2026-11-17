package db

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LLMUsage is one lightweight LLM-call metering row — the always-on usage ledger,
// distinct from llm_records (which stores full request/response bodies and is a
// gated debug feature). One row per completion call, written on both success and
// error, so token accounting is complete even for interrupted/failed runs. Carries
// only the dimensions needed to slice token spend (model / profile / task / agent),
// never any prompt or response content.
type LLMUsage struct {
	TaskID        string `json:"task_id"`        // task registry id (matches llm_records.task_id)
	ExplorationID int64  `json:"exploration_id"` // exploration id parsed from the session (0 = unknown/non-task)
	Worker        string `json:"worker"`         // agent lane: worker / planner / mainagent / goals
	Model         string `json:"model"`
	ProfileName   string `json:"profile_name"`
	LatencyMs     int    `json:"latency_ms"`
	InputTokens   int    `json:"input_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	CacheRead     int    `json:"cache_read"`
	CacheWrite    int    `json:"cache_write"`
	Status        string `json:"status"` // ok | error
}

const llmUsageSchema = `
CREATE TABLE IF NOT EXISTS llm_usage (
    id             BIGSERIAL PRIMARY KEY,
    ts             TIMESTAMPTZ NOT NULL DEFAULT now(),
    task_id        TEXT,
    exploration_id BIGINT,
    worker         TEXT,
    model          TEXT,
    profile_name   TEXT,
    latency_ms     INTEGER,
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_read     INTEGER NOT NULL DEFAULT 0,
    cache_write    INTEGER NOT NULL DEFAULT 0,
    status         TEXT
);
CREATE INDEX IF NOT EXISTS idx_llm_usage_task  ON llm_usage(task_id);
CREATE INDEX IF NOT EXISTS idx_llm_usage_task_time ON llm_usage(task_id, ts);
CREATE INDEX IF NOT EXISTS idx_llm_usage_model ON llm_usage(task_id, model);
CREATE INDEX IF NOT EXISTS idx_llm_usage_exp   ON llm_usage(exploration_id);
`

// TaskTimelineBucket is one fixed UTC interval in the task timeline. Counters
// and token sums come from llm_usage; activity-derived fields are joined by
// the same bucket start. All zero values are meaningful only when HasUsage is
// true (the corresponding interval has ledger rows).
type TaskTimelineBucket struct {
	Start            time.Time `json:"start"`
	HasUsage         bool      `json:"has_usage"`
	Calls            int       `json:"calls"`
	FailedCalls      int       `json:"failed_calls"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	PlannerRounds    int       `json:"planner_rounds"`
	WorkerResultRows int       `json:"worker_result_rows"`
}

// TaskTimelineEventMarker is a lightweight, replayable event pointer. Details
// remain in their original activity row and are opened through existing
// authorized endpoints.
type TaskTimelineEventMarker struct {
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`
	SourceID int64     `json:"source_id"`
}

// TaskTimelineResult is a read-only task timeline snapshot. usage_status is
// "recorded" when the ledger has rows, "no_rows" otherwise, and
// "unavailable" when the caller could not query the ledger.
type TaskTimelineResult struct {
	UsageStatus   string                    `json:"usage_status"`
	Buckets       []TaskTimelineBucket      `json:"buckets"`
	EventMarkers  []TaskTimelineEventMarker `json:"event_markers"`
	SessionTotals TokenUsage                `json:"session_totals"`
	HasMoreEvents bool                      `json:"has_more_events"`
}

const (
	timelineEventPageSize = 100
	timelineMaxBuckets    = 672
)

var errTimelineTooManyBuckets = errors.New("timeline range exceeds 672 buckets")

// EnsureLLMUsageTable creates the llm_usage metering table if it does not exist.
func (d *DB) EnsureLLMUsageTable() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := coordinateWithSchemaMigration(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(llmUsageSchema); err != nil {
		return err
	}
	return tx.Commit()
}

// InsertLLMUsage appends one metering row. Best-effort: callers log and continue on
// error (a lost metering row must never break the LLM call).
func (d *DB) InsertLLMUsage(u *LLMUsage) error {
	var expID any
	if u.ExplorationID > 0 {
		expID = u.ExplorationID
	}
	_, err := d.Exec(`
INSERT INTO llm_usage(task_id, exploration_id, worker, model, profile_name, latency_ms, input_tokens, output_tokens, cache_read, cache_write, status)
VALUES (NULLIF($1,''),$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),$6,$7,$8,$9,$10,$11)`,
		u.TaskID, expID, u.Worker, u.Model, u.ProfileName,
		u.LatencyMs, u.InputTokens, u.OutputTokens, u.CacheRead, u.CacheWrite, u.Status)
	return err
}

// TokenByModel aggregates a task's LLM token usage grouped by model, most-used
// first, from the always-on llm_usage ledger. taskID is the task registry id.
// Accurate even with per-agent model bindings, pool rotation/failover, and
// interrupted runs, since every call (success or error) is metered.
func (d *DB) TokenByModel(taskID string) ([]ModelTokenStat, error) {
	rows, err := d.Query(`
SELECT COALESCE(NULLIF(model,''),'(unknown)') AS model, COUNT(*) AS calls,
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0)
FROM llm_usage
WHERE COALESCE(task_id,'') = $1
GROUP BY model
ORDER BY SUM(input_tokens) + SUM(output_tokens) DESC, model`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelTokenStat{}
	for rows.Next() {
		var m ModelTokenStat
		if err := rows.Scan(&m.Model, &m.Calls, &m.InputTokens, &m.OutputTokens,
			&m.CacheReadTokens, &m.CacheWriteTokens); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// TaskTimeline aggregates one task's usage ledger and activity markers by
// fixed UTC buckets. The interval is half-open ([from,to)), and bucket
// boundaries start at from. Historical rows carrying only exploration_id are
// included only when that exploration maps to exactly one live task row.
// The SQL DISTINCT join on llm_usage.id prevents a duplicate count if a row
// has both task_id and exploration_id.
func (d *DB) TaskTimeline(taskID int64, explorationID int64, from, to time.Time, bucketSeconds int, after *TaskTimelineEventMarker) (*TaskTimelineResult, error) {
	elapsed := to.Sub(from)
	if elapsed <= 0 {
		return &TaskTimelineResult{
			UsageStatus:  "no_rows",
			Buckets:      []TaskTimelineBucket{},
			EventMarkers: []TaskTimelineEventMarker{},
		}, nil
	}
	bucketSpan := time.Duration(bucketSeconds) * time.Second
	bucketCount := int((elapsed + bucketSpan - 1) / bucketSpan)
	if bucketCount < 0 || bucketCount > timelineMaxBuckets {
		return nil, errTimelineTooManyBuckets
	}
	out := &TaskTimelineResult{
		UsageStatus:  "no_rows",
		Buckets:      make([]TaskTimelineBucket, bucketCount),
		EventMarkers: []TaskTimelineEventMarker{},
	}
	for i := range out.Buckets {
		out.Buckets[i].Start = from.Add(time.Duration(i*bucketSeconds) * time.Second)
	}
	if bucketCount == 0 {
		return out, nil
	}

	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	// REPEATABLE READ gives buckets and markers one stable read view even if
	// new usage or activity is written while the request runs.
	if _, err := tx.Exec(`SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
		return nil, err
	}

	usageRows, err := tx.Query(`
SELECT bucket_no, COUNT(*)::bigint, COUNT(*) FILTER (WHERE status='error')::bigint,
       COALESCE(SUM(input_tokens),0)::bigint, COALESCE(SUM(output_tokens),0)::bigint,
       COALESCE(SUM(cache_read),0)::bigint, COALESCE(SUM(cache_write),0)::bigint
FROM (
	SELECT DISTINCT u.id,
           floor(extract(epoch FROM (u.ts - $2)) / $3)::int AS bucket_no,
           u.input_tokens, u.output_tokens, u.cache_read, u.cache_write, u.status
    FROM llm_usage u
    LEFT JOIN tasks task ON task.exploration_id = u.exploration_id
    WHERE (COALESCE(u.task_id,'') = $1 OR u.exploration_id = $4)
      AND u.ts >= $2 AND u.ts < $5
      AND (COALESCE(u.task_id,'') = $1 OR task.id IS NOT NULL)
) usage
GROUP BY bucket_no`, strconv.FormatInt(taskID, 10), from, bucketSeconds, explorationID, to)
	if err != nil {
		return nil, err
	}
	for usageRows.Next() {
		var bucket int
		var row TaskTimelineBucket
		if err := usageRows.Scan(&bucket, &row.Calls, &row.FailedCalls, &row.InputTokens, &row.OutputTokens, &row.CacheReadTokens, &row.CacheWriteTokens); err != nil {
			usageRows.Close()
			return nil, err
		}
		if bucket < 0 || bucket >= len(out.Buckets) {
			continue
		}
		row.Start = out.Buckets[bucket].Start
		row.HasUsage = true
		out.Buckets[bucket] = row
	}
	if err := usageRows.Err(); err != nil {
		usageRows.Close()
		return nil, err
	}
	usageRows.Close()

	for _, bucket := range out.Buckets {
		if bucket.HasUsage {
			out.UsageStatus = "recorded"
			break
		}
	}

	activityRows, err := tx.Query(`
SELECT floor(extract(epoch FROM (a.created_at - $1::timestamptz)) / $2)::int AS bucket_no,
       a.worker, a.kind, a.id, a.created_at
FROM activity a
WHERE a.exploration_id = $3 AND a.created_at >= $1::timestamptz AND a.created_at < $4::timestamptz`,
		from, bucketSeconds, explorationID, to)
	if err != nil {
		return nil, err
	}
	for activityRows.Next() {
		var bucket int
		var worker, kind string
		var event TaskTimelineEventMarker
		if err := activityRows.Scan(&bucket, &worker, &kind, &event.SourceID, &event.Time); err != nil {
			activityRows.Close()
			return nil, err
		}
		if bucket >= 0 && bucket < len(out.Buckets) {
			if worker == "planner" && kind == "round" {
				out.Buckets[bucket].PlannerRounds++
			}
			if kind == "result" && (worker == "worker" || worker == "" || strings.HasPrefix(worker, "work")) {
				out.Buckets[bucket].WorkerResultRows++
			}
		}
		if kind != "round" && kind != "result" {
			continue
		}
		if after != nil &&
			(event.Time.Before(after.Time) ||
				(event.Time.Equal(after.Time) && event.SourceID <= after.SourceID)) {
			continue
		}
		event.Kind = map[bool]string{true: "planner_round", false: "worker_result"}[kind == "round"]
		out.EventMarkers = append(out.EventMarkers, event)
	}
	if err := activityRows.Err(); err != nil {
		activityRows.Close()
		return nil, err
	}
	activityRows.Close()

	// A page larger than the requested limit lets the handler report has_more
	// without a COUNT(*) over the complete history.
	sort.Slice(out.EventMarkers, func(i, j int) bool {
		if out.EventMarkers[i].Time.Equal(out.EventMarkers[j].Time) {
			return out.EventMarkers[i].SourceID < out.EventMarkers[j].SourceID
		}
		return out.EventMarkers[i].Time.Before(out.EventMarkers[j].Time)
	})
	if len(out.EventMarkers) > timelineEventPageSize+1 {
		out.EventMarkers = out.EventMarkers[:timelineEventPageSize+1]
	}
	if len(out.EventMarkers) > timelineEventPageSize {
		out.EventMarkers = out.EventMarkers[:timelineEventPageSize]
		out.HasMoreEvents = true
	}

	store := d.Exploration(explorationID)
	if totals, err := store.SessionTotalsForTimeline(tx); err == nil {
		out.SessionTotals = totals
	} else {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// ProfileUsage aggregates the whole ledger's token spend for one LLM profile
// (global, all tasks). Powers the dashboard's per-profile token card (new source).
type ProfileUsage struct {
	ProfileName      string `json:"profile_name"`
	Calls            int    `json:"calls"`
	Tasks            int    `json:"tasks"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
}

// UsageByProfile returns global token spend grouped by profile name, most-used
// first. profile_name may be empty for calls made on env/non-persisted configs.
func (d *DB) UsageByProfile() ([]ProfileUsage, error) {
	rows, err := d.Query(`
SELECT COALESCE(profile_name,'') AS profile_name, COUNT(*) AS calls,
       COUNT(DISTINCT task_id) AS tasks,
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0)
FROM llm_usage
GROUP BY profile_name
ORDER BY SUM(input_tokens) + SUM(output_tokens) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProfileUsage{}
	for rows.Next() {
		var p ProfileUsage
		if err := rows.Scan(&p.ProfileName, &p.Calls, &p.Tasks,
			&p.InputTokens, &p.OutputTokens, &p.CacheReadTokens, &p.CacheWriteTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	archived, err := d.archivedTaskAggregates()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]ProfileUsage, len(out))
	for _, current := range out {
		byName[current.ProfileName] = current
	}
	for _, aggregate := range archived {
		for _, cold := range aggregate.TokenProfiles {
			current := byName[cold.ProfileName]
			current.ProfileName = cold.ProfileName
			current.Calls += cold.Calls
			current.Tasks += cold.Tasks
			current.InputTokens += cold.InputTokens
			current.OutputTokens += cold.OutputTokens
			current.CacheReadTokens += cold.CacheReadTokens
			current.CacheWriteTokens += cold.CacheWriteTokens
			byName[cold.ProfileName] = current
		}
	}
	out = out[:0]
	for _, current := range byName {
		out = append(out, current)
	}
	sort.Slice(out, func(i, j int) bool {
		left := out[i].InputTokens + out[i].OutputTokens
		right := out[j].InputTokens + out[j].OutputTokens
		if left != right {
			return left > right
		}
		return out[i].ProfileName < out[j].ProfileName
	})
	return out, nil
}

// ProfileDayUsage is one (profile, UTC calendar day) token bucket for the daily
// chart. Unlike the activity-based chart, ts is the real call time, so this is
// actual per-day consumption rather than tokens bucketed by task creation date.
type ProfileDayUsage struct {
	ProfileName     string `json:"profile_name"`
	Date            string `json:"date"` // YYYY-MM-DD (UTC)
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	CacheReadTokens int    `json:"cache_read_tokens"`
}

// UsageDaily returns per-(profile, day) token buckets for the past `days` days
// (default 365 when days<=0), so the dashboard can slice by profile + range.
func (d *DB) UsageDaily(days int) ([]ProfileDayUsage, error) {
	if days <= 0 {
		days = 365
	}
	rows, err := d.Query(`
SELECT COALESCE(profile_name,'') AS profile_name,
       to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day,
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(cache_read),0)
FROM llm_usage
WHERE ts >= now() - ($1 * interval '1 day')
GROUP BY profile_name, day
ORDER BY day`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProfileDayUsage{}
	for rows.Next() {
		var p ProfileDayUsage
		if err := rows.Scan(&p.ProfileName, &p.Date, &p.InputTokens, &p.OutputTokens, &p.CacheReadTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	archived, err := d.archivedTaskAggregates()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	byKey := make(map[string]ProfileDayUsage, len(out))
	for _, current := range out {
		byKey[current.ProfileName+"\x00"+current.Date] = current
	}
	for _, aggregate := range archived {
		for _, cold := range aggregate.TokenDaily {
			if cold.Date < cutoff {
				continue
			}
			key := cold.ProfileName + "\x00" + cold.Date
			current := byKey[key]
			current.ProfileName = cold.ProfileName
			current.Date = cold.Date
			current.InputTokens += cold.InputTokens
			current.OutputTokens += cold.OutputTokens
			current.CacheReadTokens += cold.CacheReadTokens
			byKey[key] = current
		}
	}
	out = out[:0]
	for _, current := range byKey {
		out = append(out, current)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].ProfileName < out[j].ProfileName
	})
	return out, nil
}

// JudgeDayUsage is one UTC-day token bucket for the intercept fallback judge,
// for the config page's recent-spend sparkline.
type JudgeDayUsage struct {
	Date         string `json:"date"` // YYYY-MM-DD (UTC)
	Calls        int    `json:"calls"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

// JudgeUsage is the cumulative token spend of the intercept fallback judge
// (worker='judge' rows in the always-on ledger), plus a recent daily series.
type JudgeUsage struct {
	Calls            int             `json:"calls"`
	InputTokens      int             `json:"input_tokens"`
	OutputTokens     int             `json:"output_tokens"`
	CacheReadTokens  int             `json:"cache_read_tokens"`
	CacheWriteTokens int             `json:"cache_write_tokens"`
	Daily            []JudgeDayUsage `json:"daily"`
}

// JudgeUsageStats returns the fallback judge's all-time token totals and a
// per-day series over the past `days` days (default 30). Sourced from the
// always-on llm_usage ledger, so it is accurate across pool rotation and
// interrupted/failed judge calls. days only bounds the daily series; totals are
// all-time.
func (d *DB) JudgeUsageStats(days int) (JudgeUsage, error) {
	if days <= 0 {
		days = 30
	}
	var u JudgeUsage
	err := d.QueryRow(`
SELECT COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0)
FROM llm_usage
WHERE worker = 'judge'`).Scan(&u.Calls, &u.InputTokens, &u.OutputTokens,
		&u.CacheReadTokens, &u.CacheWriteTokens)
	if err != nil {
		return JudgeUsage{}, err
	}
	rows, err := d.Query(`
SELECT to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day,
       COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
FROM llm_usage
WHERE worker = 'judge' AND ts >= now() - ($1 * interval '1 day')
GROUP BY day
ORDER BY day`, days)
	if err != nil {
		return JudgeUsage{}, err
	}
	defer rows.Close()
	u.Daily = []JudgeDayUsage{}
	for rows.Next() {
		var day JudgeDayUsage
		if err := rows.Scan(&day.Date, &day.Calls, &day.InputTokens, &day.OutputTokens); err != nil {
			return JudgeUsage{}, err
		}
		u.Daily = append(u.Daily, day)
	}
	if err := rows.Err(); err != nil {
		return JudgeUsage{}, err
	}
	return u, nil
}

// ParseExpID turns the exploration-id segment parsed from a session string into an
// int64 (0 when empty/non-numeric, e.g. chat sessions keyed by conversation id).
func ParseExpID(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
