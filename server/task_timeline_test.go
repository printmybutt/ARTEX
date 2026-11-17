package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestTaskTimelineAPIContract(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	task, err := m.CreateTask("timeline api", "timeline goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	numericTaskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteTask(numericTaskID) })

	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())
	t.Cleanup(cancel)
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	request := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	for _, tc := range []struct {
		path string
		code int
		text string
	}{
		{"/api/tasks/missing/timeline", http.StatusNotFound, ""},
		{"/api/tasks/" + task.ID + "/timeline?bucket_seconds=60", http.StatusBadRequest, "bucket_seconds"},
		{
			"/api/tasks/" + task.ID + "/timeline?from=" + time.Now().UTC().Add(time.Hour).Format(time.RFC3339) +
				"&to=" + time.Now().UTC().Format(time.RFC3339),
			http.StatusBadRequest, "from must be earlier",
		},
		{"/api/tasks/" + task.ID + "/timeline?bucket_seconds=3600", http.StatusOK, ""},
	} {
		rec := request(tc.path)
		if rec.Code != tc.code || (tc.text != "" && !strings.Contains(rec.Body.String(), tc.text)) {
			t.Fatalf("path=%s status=%d body=%s", tc.path, rec.Code, rec.Body.String())
		}
	}

	rec := request("/api/tasks/" + task.ID + "/timeline")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		AsOf          time.Time               `json:"as_of"`
		From          time.Time               `json:"from"`
		To            time.Time               `json:"to"`
		BucketSeconds int                     `json:"bucket_seconds"`
		UsageStatus   string                  `json:"usage_status"`
		Buckets       []db.TaskTimelineBucket `json:"buckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.AsOf.After(time.Time{}) || !response.To.Equal(response.AsOf) ||
		!response.From.Equal(response.AsOf.Add(-48*time.Hour)) || response.BucketSeconds != 3600 ||
		response.UsageStatus != "no_rows" || len(response.Buckets) != 48 {
		t.Fatalf("unexpected timeline response: %+v", response)
	}
	if _, err := m.pg.Exec(`INSERT INTO llm_usage(task_id, exploration_id, input_tokens, output_tokens) VALUES ($1,$2,123,7)`,
		strconv.FormatInt(numericTaskID, 10), task.ExpID); err != nil {
		t.Fatal(err)
	}
	rec = request("/api/tasks/" + task.ID + "/timeline")
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.UsageStatus != "recorded" || response.Buckets[len(response.Buckets)-1].InputTokens != 123 {
		t.Fatalf("ledger row missing from timeline: %+v", response)
	}
}
