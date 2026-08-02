import { useEffect, useRef, useState } from "react";
import { ScrollText, Trash2 } from "lucide-react";
import { getLogs, isDemoMode } from "../lib/api";
import { fromLogs, LogEntry } from "../lib/types";
import { Button, Card } from "../components/ui";

const LEVEL_COLOR: Record<LogEntry["level"], string> = {
  debug: "text-slate-500 dark:text-slate-500",
  info: "text-slate-700 dark:text-slate-300",
  warn: "text-amber-600 dark:text-amber-400",
  error: "text-red-600 dark:text-red-400",
};

export default function LogsPage() {
  const [logs, setLogs] = useState<LogEntry[]>([]);
  const [follow, setFollow] = useState(true);
  const [demo, setDemo] = useState(false);
  const boxRef = useRef<HTMLDivElement>(null);
  const firstRun = useRef(true);

  useEffect(() => {
    void isDemoMode().then(setDemo);
    let alive = true;
    const tick = async () => {
      try {
        const entries = fromLogs(await getLogs(200));
        if (alive) {
          setLogs((prev) => {
            // Only replace when the tail differs (avoids clobbering on no-op polls).
            if (prev.length === entries.length) return prev;
            return entries;
          });
        }
      } catch {
        /* transient poll failure: keep previous logs */
      }
    };
    void tick();
    const id = setInterval(tick, 1000);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, []);

  useEffect(() => {
    if (follow && boxRef.current) {
      boxRef.current.scrollTop = boxRef.current.scrollHeight;
    }
    firstRun.current = false;
  }, [logs, follow]);

  return (
    <div className="space-y-4">
      <Card
        title="运行日志"
        action={
          <div className="flex items-center gap-4">
            {demo && <span className="text-xs text-slate-400">演示模式</span>}
            <label className="flex cursor-pointer items-center gap-1.5 text-xs text-slate-600 dark:text-slate-300">
              <input
                type="checkbox"
                checked={follow}
                onChange={(e) => setFollow(e.target.checked)}
                className="h-3.5 w-3.5 accent-orange-500"
              />
              自动滚动
            </label>
            <Button variant="ghost" onClick={() => setLogs([])} className="!px-2 !py-1 text-xs">
              <Trash2 className="h-3.5 w-3.5" /> 清空
            </Button>
          </div>
        }
      >
        <div
          ref={boxRef}
          className="h-[420px] overflow-auto rounded-lg border border-slate-200 bg-slate-50 p-3 font-mono text-xs leading-5 dark:border-slate-800 dark:bg-slate-950"
        >
          {logs.length === 0 ? (
            <p className="text-slate-500 dark:text-slate-400">暂无日志</p>
          ) : (
            logs.map((l, i) => (
              <div key={i} className="whitespace-pre-wrap break-all">
                <span className="text-slate-400 dark:text-slate-500">{l.time}</span>{" "}
                <span className={`font-semibold uppercase ${LEVEL_COLOR[l.level]}`}>
                  {l.level}
                </span>{" "}
                <span className={LEVEL_COLOR[l.level]}>{l.msg}</span>
              </div>
            ))
          )}
        </div>
        <p className="mt-3 flex items-center gap-1.5 text-xs text-slate-500 dark:text-slate-400">
          <ScrollText className="h-3.5 w-3.5" />
          每秒刷新，最多显示最近 200 条
        </p>
      </Card>
    </div>
  );
}
