import { useEffect, useMemo, useRef, useState } from "react";
import { Save, RefreshCw, FileText } from "lucide-react";
import { getRules, saveRules, reloadRules } from "../lib/api";
import { Button, Card } from "../components/ui";

export default function RulesPage() {
  const [text, setText] = useState("");
  const [dirty, setDirty] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const taRef = useRef<HTMLTextAreaElement>(null);

  // Load once + auto-refresh every 2s, but never clobber unsaved edits.
  useEffect(() => {
    let alive = true;
    const tick = async () => {
      if (dirty) return;
      try {
        const rules = await getRules();
        if (alive) {
          setText(rules);
          setError(null);
        }
      } catch (e) {
        if (alive) setError(String(e));
      }
    };
    void tick();
    const id = setInterval(tick, 2000);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, [dirty]);

  const lines = useMemo(() => text.split("\n"), [text]);
  const ruleCount = useMemo(() => {
    let n = 0;
    for (const ln of lines) {
      const s = ln.trim();
      if (s === "" || s.startsWith("#")) continue;
      if (s.startsWith("proxy,") || s.startsWith("direct,")) n++;
    }
    return n;
  }, [lines]);

  const onSave = async () => {
    setBusy("save");
    setError(null);
    setNotice(null);
    try {
      await saveRules(text);
      setDirty(false);
      setNotice("规则已保存");
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(null);
    }
  };

  const onReload = async () => {
    setBusy("reload");
    setError(null);
    setNotice(null);
    try {
      await reloadRules();
      const rules = await getRules();
      setText(rules);
      setDirty(false);
      setNotice("规则已热重载");
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="space-y-4">
      <Card
        title="路由规则"
        action={
          <span className="inline-flex items-center gap-1.5 text-xs text-slate-500 dark:text-slate-400">
            <FileText className="h-3.5 w-3.5" />
            共 {ruleCount} 条规则{dirty ? "（有未保存修改）" : ""}
          </span>
        }
      >
        <div className="grid grid-cols-1 gap-3 md:grid-cols-[auto_1fr]">
          {/* Line numbers */}
          <div
            aria-hidden
            className="hidden select-none overflow-hidden rounded-l-lg border border-r-0 border-slate-300 bg-slate-50 px-2 py-3 text-right font-mono text-xs leading-5 text-slate-400 md:block dark:border-slate-700 dark:bg-slate-800"
          >
            {lines.map((_, i) => (
              <div key={i}>{i + 1}</div>
            ))}
          </div>
          <textarea
            ref={taRef}
            value={text}
            onChange={(e) => {
              setText(e.target.value);
              setDirty(true);
              setNotice(null);
            }}
            spellCheck={false}
            className="min-h-[320px] w-full resize-y rounded-lg border border-slate-300 bg-white px-3 py-2 font-mono text-xs leading-5 text-slate-900 focus:border-orange-500 focus:outline-none focus:ring-1 focus:ring-orange-500/40 md:rounded-l-none dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100"
            placeholder="proxy,geosite:google&#10;direct,geoip:cn&#10;# 空行与 # 开头的行会被忽略"
          />
        </div>

        <div className="mt-4 flex flex-wrap items-center gap-3">
          <Button onClick={onSave} loading={busy === "save"} disabled={!dirty}>
            <Save className="h-4 w-4" /> 保存
          </Button>
          <Button
            onClick={onReload}
            variant="secondary"
            loading={busy === "reload"}
          >
            <RefreshCw className="h-4 w-4" /> 重新加载
          </Button>
          {notice && (
            <span className="text-sm text-emerald-600 dark:text-emerald-400">{notice}</span>
          )}
          {error && <span className="text-sm text-red-600 dark:text-red-400">{error}</span>}
        </div>
        <p className="mt-3 text-xs text-slate-500 dark:text-slate-400">
          语法：每行一条 <code className="font-mono">行为,条件</code>。行为为{" "}
          <code className="font-mono">proxy</code>（走隧道）或{" "}
          <code className="font-mono">direct</code>（直连）；条件支持{" "}
          <code className="font-mono">geosite:&lt;name&gt;</code>、
          <code className="font-mono">geoip:&lt;cc&gt;</code>、
          <code className="font-mono">geoip:private</code>、
          <code className="font-mono">geoip:lan</code>、
          <code className="font-mono">domain:&lt;suffix&gt;</code>。未匹配默认直连。
        </p>
      </Card>
    </div>
  );
}
