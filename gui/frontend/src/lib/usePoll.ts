// usePoll 是共享轮询 hook：setInterval + 自动清理 + alive 守卫。
// 提取自 StatusPage 的内联实现，供 StatusPage / LogsPage / RulesPage 共用。
//
// 用法：const { data, error } = usePoll(getStatus, 2000, [refreshKey]);
// deps 变化时重建轮询（类似 React 的 effect deps 语义）。

import { useEffect, useState } from "react";

export function usePoll<T>(
  fn: () => Promise<T>,
  ms: number,
  deps: unknown[] = [],
): { data: T | null; error: string | null } {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    const tick = async () => {
      try {
        const v = await fn();
        if (alive) {
          setData(v);
          setError(null);
        }
      } catch (e) {
        if (alive) setError(String(e));
      }
    };
    void tick();
    const id = setInterval(tick, ms);
    return () => {
      alive = false;
      clearInterval(id);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  return { data, error };
}
