import { useState } from "react";
import { Radar, Check } from "lucide-react";
import { applyEdge, scanEdgesV4, scanEdgesV6 } from "../lib/api";
import { Button, Card, StatusPill } from "../components/ui";

interface ScanResult {
  family: "v4" | "v6";
  edges: string[];
}

export default function ScanPage() {
  const [busy, setBusy] = useState<"v4" | "v6" | null>(null);
  const [results, setResults] = useState<ScanResult[]>([]);
  const [applied, setApplied] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [noticeKind, setNoticeKind] = useState<"info" | "warn">("info");

  const onScan = async (family: "v4" | "v6") => {
    setBusy(family);
    setError(null);
    setNotice(null);
    try {
      const edges =
        family === "v4" ? await scanEdgesV4() : await scanEdgesV6();
      setResults((rs) => {
        const rest = rs.filter((r) => r.family !== family);
        return [...rest, { family, edges }];
      });
      if (edges.length === 0) {
        setNoticeKind("warn");
        setNotice(
          "未扫描到可用端点：可能是当前网络限制了 QUIC，或注册信息缺少边缘地址。请回到状态页确认已注册，或重新注册后重试。"
        );
      }
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(null);
    }
  };

  const onApply = async (addr: string) => {
    setError(null);
    setNotice(null);
    try {
      await applyEdge(addr);
      setApplied(addr);
      setNoticeKind("info");
      setNotice(`已应用边缘 ${addr}（下次启动生效）`);
    } catch (e) {
      setError(String(e));
    }
  };

  return (
    <div className="space-y-4">
      <Card
        title="边缘扫描"
        action={
          <span className="text-xs text-slate-500 dark:text-slate-400">
            扫描 WARP 边缘，选择延迟最优的端点
          </span>
        }
      >
        <div className="flex flex-wrap gap-3">
          <Button onClick={() => onScan("v4")} loading={busy === "v4"}>
            <Radar className="h-4 w-4" /> 扫描 IPv4 边缘
          </Button>
          <Button onClick={() => onScan("v6")} loading={busy === "v6"} variant="secondary">
            <Radar className="h-4 w-4" /> 扫描 IPv6 边缘
          </Button>
          {notice && (
            <span
              className={`self-center rounded-lg px-4 py-3 text-sm ${
                noticeKind === "warn"
                  ? "bg-amber-50 text-amber-800 dark:bg-amber-950/40 dark:text-amber-300"
                  : "text-emerald-600 dark:text-emerald-400"
              }`}
            >
              {notice}
            </span>
          )}
          {error && (
            <span className="self-center text-sm text-red-600 dark:text-red-400">
              {error}
            </span>
          )}
        </div>

        {results.length === 0 && !busy && (
          <p className="mt-4 text-sm text-slate-400">
            尚无扫描结果。点击上方按钮开始扫描（需已注册 WARP）。
          </p>
        )}

        {results.map(({ family, edges }) => (
          <div key={family} className="mt-5">
            <p className="mb-2 flex items-center gap-2 text-sm font-medium">
              IPv{family} 扫描结果
              <StatusPill ok={edges.length > 0} text={edges.length > 0 ? `${edges.length} 个端点` : "无结果"} />
            </p>
            <ul className="space-y-1.5">
              {edges.map((addr, i) => (
                <li
                  key={addr}
                  className="flex items-center justify-between gap-3 rounded-lg border border-slate-200 px-3 py-2 dark:border-slate-800"
                >
                  <span className="flex items-center gap-2 font-mono text-sm">
                    {i + 1}. {addr}
                    {applied === addr && (
                      <span className="flex items-center gap-1 text-xs text-emerald-600 dark:text-emerald-400">
                        <Check className="h-3.5 w-3.5" /> 已应用
                      </span>
                    )}
                  </span>
                  <Button
                    onClick={() => onApply(addr)}
                    variant="secondary"
                    className="h-8 shrink-0 px-3 text-xs"
                    disabled={applied === addr}
                  >
                    应用
                  </Button>
                </li>
              ))}
            </ul>
          </div>
        ))}
      </Card>
    </div>
  );
}
