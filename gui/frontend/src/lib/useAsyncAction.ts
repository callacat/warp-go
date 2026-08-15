// useAsyncAction 统一异步操作的 busy / error / notice 三件套。
// 提取自 StatusPage / RulesPage / GeoPage / SettingsPage 的重复模式。
//
// 用法：
// const { busy, error, notice, run, clear } = useAsyncAction();
// await run("save", async () => { await saveRules(text); }, "规则已保存");
// // busy === "save" 时按钮 loading；error / notice 直接渲染

import { useState, useCallback } from "react";

export function useAsyncAction() {
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const run = useCallback(
    async <T>(
      key: string,
      fn: () => Promise<T>,
      successMsg?: string,
    ): Promise<T | undefined> => {
      setBusy(key);
      setError(null);
      setNotice(null);
      try {
        const result = await fn();
        if (successMsg) setNotice(successMsg);
        return result;
      } catch (e) {
        setError(String(e));
        return undefined;
      } finally {
        setBusy(null);
      }
    },
    [],
  );

  const clear = useCallback(() => {
    setError(null);
    setNotice(null);
  }, []);

  return { busy, error, notice, run, clear, setNotice, setError };
}
