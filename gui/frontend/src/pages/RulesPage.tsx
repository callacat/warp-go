import { useEffect, useMemo, useRef, useState } from "react";
import { Save, RefreshCw, FileText } from "lucide-react";
import { getRules, saveRules, reloadRules } from "../lib/api";
import { Button, Card } from "../components/ui";
import { usePoll } from "../lib/usePoll";
import { useAsyncAction } from "../lib/useAsyncAction";
import { createCodeMirrorEditor, type CodeMirrorEditor } from "../lib/codeMirror";

export default function RulesPage() {
  const [text, setText] = useState("");
  const [dirty, setDirty] = useState(false);
  const { busy, error, notice, run, clear } = useAsyncAction();
  const editorHostRef = useRef<HTMLDivElement>(null);
  const editorRef = useRef<CodeMirrorEditor | null>(null);
  const dirtyRef = useRef(false);

  // 轮询拉取规则：dirty 时不覆盖编辑器
  const { data: rulesText } = usePoll(
    async () => {
      if (dirtyRef.current) return null;
      try {
        return await getRules();
      } catch {
        return null;
      }
    },
    2000,
  );

  // 从轮询数据同步到编辑器（仅非 dirty 时）
  useEffect(() => {
    if (rulesText != null && !dirtyRef.current) {
      setText(rulesText);
      editorRef.current?.setText(rulesText);
    }
  }, [rulesText]);

  // 初始化 CodeMirror 编辑器
  useEffect(() => {
    if (!editorHostRef.current) return;
    const editor = createCodeMirrorEditor(
      editorHostRef.current,
      text,
      (newText) => {
        setText(newText);
        dirtyRef.current = true;
        setDirty(true);
        clear();
      },
    );
    editorRef.current = editor;
    return () => {
      editor.destroy();
      editorRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const lines = useMemo(() => text.split("\n"), [text]);
  const ruleCount = useMemo(() => {
    let n = 0;
    for (const ln of lines) {
      const s = ln.trim();
      if (s === "" || s.startsWith("#")) continue;
      // 计数所有有效行为行：proxy / direct / REJECT（不区分大小写）
      if (/^(proxy|direct|reject)\s*,/i.test(s)) n++;
    }
    return n;
  }, [lines]);

  const onSave = async () => {
    const ok = await run("save", async () => {
      await saveRules(text);
    }, "规则已保存");
    if (ok !== undefined) {
      dirtyRef.current = false;
      setDirty(false);
    }
  };

  const onReload = async () => {
    const ok = await run("reload", async () => {
      await reloadRules();
      const rules = await getRules();
      setText(rules);
      editorRef.current?.setText(rules);
    }, "规则已热重载");
    if (ok !== undefined) {
      dirtyRef.current = false;
      setDirty(false);
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
        {/* CodeMirror 编辑器宿主 */}
        <div
          ref={editorHostRef}
          className="min-h-[320px] w-full overflow-hidden rounded-lg border border-slate-300 bg-white dark:border-slate-700 dark:bg-slate-800"
        />

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
          <code className="font-mono">proxy</code>（走隧道）、
          <code className="font-mono">direct</code>（直连）或{" "}
          <code className="font-mono">REJECT</code>（拦截）；条件支持{" "}
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
