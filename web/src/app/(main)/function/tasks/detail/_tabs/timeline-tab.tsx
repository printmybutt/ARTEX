"use client";

import * as React from "react";

import { ClockIcon, LoaderIcon, RefreshCwIcon, TriangleAlertIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import { api } from "@/lib/api";
import type { TaskNode, TaskTimeline } from "@/lib/types";

const BUCKET_OPTIONS = [
  { value: 900, label: "15 分钟" },
  { value: 3600, label: "1 小时" },
  { value: 21600, label: "6 小时" },
  { value: 86400, label: "1 天" },
];

const USAGE_STATUS_TEXT: Record<TaskTimeline["usage_status"], string> = {
  recorded: "区间内有逐调用账本记录",
  no_rows: "区间内账本无行，用量字段为未知，不代表零",
  unavailable: "账本暂时不可读取",
};

function markerHref(taskId: string, marker: TaskTimeline["event_markers"][number], intentIDs: Set<string>): string {
  if (marker.kind === "planner_round") {
    return `/function/tasks/detail/?id=${encodeURIComponent(taskId)}&tab=sessions&session=plan`;
  }
  const session = intentIDs.has(String(marker.source_id)) ? `intent:${marker.source_id}` : null;
  if (!session) return "";
  return `/function/tasks/detail/?id=${encodeURIComponent(taskId)}&tab=sessions&session=${encodeURIComponent(session)}`;
}

function formatTokens(value: number | null | undefined): string {
  if (value == null) return "—";
  if (value >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(1)}B`;
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}k`;
  return String(value);
}

function formatTime(value: string, bucketSeconds: number): string {
  const date = new Date(value);
  return date.toLocaleString("zh-CN", {
    hour12: false,
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    ...(bucketSeconds >= 86400 ? { year: "numeric" } : {}),
  });
}

export function TimelineTab({ taskId }: { taskId: string }) {
  const [timeline, setTimeline] = React.useState<TaskTimeline | null>(null);
  const [bucketSeconds, setBucketSeconds] = React.useState(3600);
  const [cursor, setCursor] = React.useState<string | undefined>(undefined);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState("");
  const [intents, setIntents] = React.useState<TaskNode[]>([]);

  React.useEffect(() => {
    api
      .intents(taskId)
      .then(setIntents)
      .catch(() => setIntents([]));
  }, [taskId]);

  const load = React.useCallback(
    async (nextBucketSeconds = bucketSeconds, nextCursor = cursor) => {
      setLoading(true);
      setError("");
      try {
        const response = await api.taskTimeline(taskId, {
          bucket_seconds: nextBucketSeconds,
          cursor: nextCursor,
        });
        setTimeline(response);
      } catch (e) {
        setError(e instanceof Error ? e.message : "时间线加载失败");
      } finally {
        setLoading(false);
      }
    },
    [bucketSeconds, cursor, taskId],
  );

  React.useEffect(() => {
    setCursor(undefined);
    void load(bucketSeconds, undefined);
  }, [bucketSeconds, load]);

  const maxInput = Math.max(1, ...(timeline?.buckets.map((b) => b.input_tokens ?? 0) ?? [1]));
  const markers = timeline?.event_markers ?? [];
  const nextCursor =
    timeline?.has_more_events && timeline.next_cursor
      ? `${timeline.next_cursor.time}|${timeline.next_cursor.source_id}|${timeline.next_cursor.kind}`
      : undefined;
  const intentIDs = new Set(intents.map((intent) => intent.id));

  return (
    <div className="grid gap-4">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <ClockIcon className="size-4 text-blue-500" /> 用量与事件时间线
          </CardTitle>
          <CardDescription>
            逐调用账本按 UTC 固定时间桶聚合；缓存读已包含在输入中，不会额外相加。默认显示最近 48
            小时，当前版可再选择粒度。
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <div className="flex flex-wrap items-center gap-2">
            <NativeSelect
              size="sm"
              value={String(bucketSeconds)}
              onChange={(e) => setBucketSeconds(Number(e.target.value))}
              aria-label="时间桶粒度"
            >
              {BUCKET_OPTIONS.map((option) => (
                <NativeSelectOption key={option.value} value={String(option.value)}>
                  {option.label}
                </NativeSelectOption>
              ))}
            </NativeSelect>
            <Button size="sm" variant="outline" onClick={() => void load()} disabled={loading}>
              {loading ? <LoaderIcon className="size-3.5 animate-spin" /> : <RefreshCwIcon className="size-3.5" />}
              刷新
            </Button>
            {timeline && (
              <span className="text-muted-foreground text-xs">
                {formatTime(timeline.from, bucketSeconds)} – {formatTime(timeline.to, bucketSeconds)} · 共{" "}
                {timeline.buckets.length} 桶
              </span>
            )}
          </div>

          {error ? (
            <div className="flex items-center gap-2 rounded-md border border-destructive/50 bg-destructive/10 px-3 py-2 text-destructive text-sm">
              <TriangleAlertIcon className="size-4" /> {error}
            </div>
          ) : (
            <div className="text-muted-foreground text-xs">
              {timeline ? USAGE_STATUS_TEXT[timeline.usage_status] : "加载中"}
            </div>
          )}

          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b text-left text-muted-foreground text-xs">
                  <th className="py-1.5 pr-3 font-medium">时间桶</th>
                  <th className="py-1.5 pr-3 text-right font-medium">调用</th>
                  <th className="py-1.5 pr-3 text-right font-medium">失败</th>
                  <th className="py-1.5 pr-3 text-right font-medium">输入</th>
                  <th className="py-1.5 pr-3 text-right font-medium">输出</th>
                  <th className="py-1.5 pr-3 text-right font-medium">缓存读</th>
                  <th className="py-1.5 pr-3 text-right font-medium">缓存写入</th>
                  <th className="py-1.5 pr-3 text-right font-medium">规划轮</th>
                  <th className="py-1.5 text-right font-medium">Worker 结果</th>
                </tr>
              </thead>
              <tbody>
                {(timeline?.buckets ?? []).map((bucket) => (
                  <tr key={bucket.start} className="border-b last:border-0">
                    <td className="whitespace-nowrap py-1.5 pr-3">{formatTime(bucket.start, bucketSeconds)}</td>
                    <td className="py-1.5 pr-3 text-right tabular-nums">{bucket.has_usage ? bucket.calls : "—"}</td>
                    <td className="py-1.5 pr-3 text-right tabular-nums">
                      <span className={bucket.failed_calls > 0 ? "text-red-500" : ""}>
                        {bucket.has_usage ? bucket.failed_calls : "—"}
                      </span>
                    </td>
                    <td className="py-1.5 pr-3 text-right tabular-nums">{formatTokens(bucket.input_tokens)}</td>
                    <td className="py-1.5 pr-3 text-right tabular-nums">{formatTokens(bucket.output_tokens)}</td>
                    <td className="py-1.5 pr-3 text-right tabular-nums">{formatTokens(bucket.cache_read_tokens)}</td>
                    <td className="py-1.5 pr-3 text-right tabular-nums">{formatTokens(bucket.cache_write_tokens)}</td>
                    <td className="py-1.5 pr-3 text-right tabular-nums">{bucket.planner_rounds}</td>
                    <td className="py-1.5 text-right tabular-nums">{bucket.worker_result_rows}</td>
                    <td className="hidden py-1.5 pl-2">
                      <div className="h-1.5 w-20 rounded bg-muted">
                        <div
                          className="h-1.5 rounded bg-blue-500"
                          style={{ width: `${Math.round(((bucket.input_tokens ?? 0) / maxInput) * 100)}%` }}
                        />
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {timeline && (
            <div className="grid gap-1 rounded-md border p-3 text-xs">
              <span className="font-medium">会话汇总（activity.result 旧口径）</span>
              <span className="text-muted-foreground">
                输入 {formatTokens(timeline.session_totals.input_tokens)} · 输出{" "}
                {formatTokens(timeline.session_totals.output_tokens)} · 缓存读{" "}
                {formatTokens(timeline.session_totals.cache_read_tokens)} · 缓存写入{" "}
                {formatTokens(timeline.session_totals.cache_write_tokens)}
              </span>
              <span className="text-muted-foreground">
                该口径与逐调用账本独立对账，不参与时间桶相加。账本缺行时时间桶为未知。
              </span>
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">事件标记</CardTitle>
          <CardDescription>按时间和来源编号分页，规划轮直接进入既有 Plan 会话。</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {loading && !timeline && <p className="text-muted-foreground text-sm">加载中</p>}
          {timeline && markers.length === 0 && (
            <p className="text-muted-foreground text-sm">区间内没有可回放的规划轮或 Worker 结束事件。</p>
          )}
          {markers.length > 0 && (
            <div className="flex flex-col divide-y">
              {markers.map((marker) => (
                <a
                  key={`${marker.time}-${marker.kind}-${marker.source_id}`}
                  href={markerHref(taskId, marker, intentIDs)}
                  className={
                    markerHref(taskId, marker, intentIDs)
                      ? "flex items-center gap-3 py-2 text-sm hover:underline"
                      : "flex items-center gap-3 py-2 text-sm"
                  }
                >
                  <span className="w-36 shrink-0 text-muted-foreground tabular-nums">
                    {formatTime(marker.time, bucketSeconds)}
                  </span>
                  <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs">
                    {marker.kind === "planner_round" ? "规划轮" : "Worker 结束"}
                  </span>
                  <span className="truncate font-mono text-muted-foreground text-xs">activity #{marker.source_id}</span>
                  <span className="text-muted-foreground ml-auto shrink-0 text-xs">
                    {markerHref(taskId, marker, intentIDs) ? "打开会话" : "定位到会话 Tab"}
                  </span>
                </a>
              ))}
            </div>
          )}
          {timeline?.has_more_events && nextCursor && (
            <Button
              size="sm"
              variant="outline"
              disabled={loading}
              onClick={() => {
                setCursor(nextCursor);
                void load(bucketSeconds, nextCursor);
              }}
            >
              加载更多
            </Button>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
