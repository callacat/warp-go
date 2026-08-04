import type { LogEntry } from "./types";

/**
 * 判定两批日志尾条是否不同——供日志页轮询去重用。
 *
 * 不能只比长度：一旦到达环形缓冲上限（200），新日志顶替最旧条目而长度不变。
 * 若仅比较 length，达到上限后日志有新增但页面不再刷新（经典 bug）。比较尾条
 * (time+level+msg) 才既能在无变化时不触发重渲染，又能在内容变化时正确刷新。
 */
export function logsTailChanged(prev: LogEntry[], entries: LogEntry[]): boolean {
  const a = prev[prev.length - 1];
  const b = entries[entries.length - 1];
  if (prev.length === 0 && entries.length === 0) return false;
  if (a && b && a.time === b.time && a.level === b.level && a.msg === b.msg) {
    return false;
  }
  return true;
}