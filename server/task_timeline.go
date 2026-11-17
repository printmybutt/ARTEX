package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

const (
	taskTimelineMaxBuckets = 672
	taskTimelineDefaultTo  = 48 * time.Hour
)

var taskTimelineAllowedBuckets = map[int]bool{
	900:   true,
	3600:  true,
	21600: true,
	86400: true,
}

type taskTimelineResponse struct {
	AsOf          time.Time                    `json:"as_of"`
	From          time.Time                    `json:"from"`
	To            time.Time                    `json:"to"`
	BucketSeconds int                          `json:"bucket_seconds"`
	UsageStatus   string                       `json:"usage_status"`
	Buckets       []db.TaskTimelineBucket      `json:"buckets"`
	EventMarkers  []db.TaskTimelineEventMarker `json:"event_markers"`
	SessionTotals db.TokenUsage                `json:"session_totals"`
	HasMoreEvents bool                         `json:"has_more_events"`
	NextCursor    *db.TaskTimelineEventMarker  `json:"next_cursor,omitempty"`
}

func parseTaskTimelineTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil || t.Location() == nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// taskTimeline returns the P0 read-only replay view. All data is computed from
// one PostgreSQL repeatable-read snapshot; marker detail stays behind existing
// authorized activity endpoints.
func (s *Server) taskTimeline(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if s.m.pg == nil {
		writeErr(w, http.StatusServiceUnavailable, "timeline storage unavailable")
		return
	}

	query := r.URL.Query()
	bucketSeconds := 3600
	if raw := query.Get("bucket_seconds"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || !taskTimelineAllowedBuckets[parsed] {
			writeErr(w, http.StatusBadRequest, "bucket_seconds must be 900, 3600, 21600, or 86400")
			return
		}
		bucketSeconds = parsed
	}

	from, fromPresent := parseTaskTimelineTime(query.Get("from"))
	to, toPresent := parseTaskTimelineTime(query.Get("to"))
	if (query.Get("from") != "" && !fromPresent) || (query.Get("to") != "" && !toPresent) {
		writeErr(w, http.StatusBadRequest, "from and to must be UTC RFC 3339 timestamps")
		return
	}
	if fromPresent && toPresent && !from.Before(to) {
		writeErr(w, http.StatusBadRequest, "from must be earlier than to")
		return
	}

	asOf := time.Now().UTC()
	defaultFrom := asOf.Add(-taskTimelineDefaultTo)
	if !toPresent {
		if lifecycle := t.lifecycleSnapshot(); lifecycle.CompletedAt > 0 {
			completed := time.Unix(lifecycle.CompletedAt, 0).UTC()
			if completed.Before(asOf) {
				asOf = completed
			}
		}
		to = asOf
		if !fromPresent {
			from = asOf.Add(-taskTimelineDefaultTo)
		}
	}
	if !fromPresent {
		if to.IsZero() {
			from = defaultFrom
		} else {
			from = to.Add(-taskTimelineDefaultTo)
		}
	}
	bucketSpan := time.Duration(bucketSeconds) * time.Second
	bucketCount := int((to.Sub(from) + bucketSpan - 1) / bucketSpan)
	if bucketCount < 0 || bucketCount > taskTimelineMaxBuckets {
		writeErr(w, http.StatusBadRequest, "requested range exceeds 672 timeline buckets")
		return
	}

	var after *db.TaskTimelineEventMarker
	if raw := query.Get("cursor"); raw != "" {
		parts := strings.SplitN(raw, "|", 3)
		if len(parts) != 3 {
			writeErr(w, http.StatusBadRequest, "invalid timeline cursor")
			return
		}
		cursorTime, valid := parseTaskTimelineTime(parts[0])
		if !valid || (parts[2] != "planner_round" && parts[2] != "worker_result") {
			writeErr(w, http.StatusBadRequest, "invalid timeline cursor")
			return
		}
		sourceID, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || sourceID <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid timeline cursor")
			return
		}
		after = &db.TaskTimelineEventMarker{Time: cursorTime, Kind: parts[2], SourceID: sourceID}
	}

	numericTaskID, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil || numericTaskID <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid task id")
		return
	}
	result, err := s.m.pg.TaskTimeline(numericTaskID, t.ExpID, from, to, bucketSeconds, after)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "timeline query failed: "+err.Error())
		return
	}
	response := taskTimelineResponse{
		AsOf:          asOf,
		From:          from,
		To:            to,
		BucketSeconds: bucketSeconds,
		UsageStatus:   result.UsageStatus,
		Buckets:       result.Buckets,
		EventMarkers:  result.EventMarkers,
		SessionTotals: result.SessionTotals,
		HasMoreEvents: result.HasMoreEvents,
	}
	if response.Buckets == nil {
		response.Buckets = []db.TaskTimelineBucket{}
	}
	if len(result.EventMarkers) > 0 {
		last := result.EventMarkers[len(result.EventMarkers)-1]
		response.NextCursor = &db.TaskTimelineEventMarker{
			Time:     last.Time,
			Kind:     last.Kind,
			SourceID: last.SourceID,
		}
	}
	writeJSON(w, http.StatusOK, response)
}
