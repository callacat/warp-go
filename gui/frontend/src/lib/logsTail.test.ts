import { describe, it, expect } from "vitest";
import { logsTailChanged } from "./logsTail";
import type { LogEntry } from "./types";

const e = (time: string, msg: string, level: LogEntry["level"] = "info"): LogEntry => ({
  time,
  level,
  msg,
});

describe("logsTailChanged", () => {
  it("returns false when both sides are empty", () => {
    expect(logsTailChanged([], [])).toBe(false);
  });

  it("returns true when prev is empty and entries have logs", () => {
    expect(logsTailChanged([], [e("10:00:00", "hello")])).toBe(true);
  });

  it("returns true when the tail message differs", () => {
    const prev = [e("10:00:00", "a")];
    const next = [e("10:00:00", "a"), e("10:00:01", "b")];
    expect(logsTailChanged(prev, next)).toBe(true);
  });

  it("returns false when the tail is identical", () => {
    const prev = [e("10:00:00", "a")];
    expect(logsTailChanged(prev, [e("10:00:00", "a")])).toBe(false);
  });

  it("returns true when the tail differs only by level", () => {
    const prev = [e("10:00:00", "a", "info")];
    const next = [e("10:00:00", "a", "warn")];
    expect(logsTailChanged(prev, next)).toBe(true);
  });

  it("returns true when the tail differs only by time", () => {
    const prev = [e("10:00:00", "a")];
    const next = [e("10:00:01", "a")];
    expect(logsTailChanged(prev, next)).toBe(true);
  });

  it("returns true when prev is longer but the tail is a new entry", () => {
    // The real bug: at the 200-cap, new logs replace oldest but length stays 200,
    // so length-only comparison misses updates. Tail comparison must catch it.
    const prev: LogEntry[] = Array.from({ length: 200 }, (_, i) => e(`10:00:${i}`, `msg-${i}`));
    const next: LogEntry[] = [
      ...prev.slice(1), // drop oldest, keep 199
      e("10:03:20", "brand-new-log"), // append new tail
    ];
    expect(next.length).toBe(200); // same length as prev
    expect(logsTailChanged(prev, next)).toBe(true);
  });

  it("returns false when both batches are identical 200-cap snapshots", () => {
    // A monotonic ring buffer only changes the tail when new logs arrive; a
    // no-op poll returns the exact same batch, so no refresh is needed.
    const batch: LogEntry[] = Array.from({ length: 200 }, (_, i) =>
      e(`10:00:${i}`, `msg-${i}`),
    );
    expect(logsTailChanged(batch, [...batch])).toBe(false);
  });

  it("handles partial-entry tails (missing msg)", () => {
    const prev: LogEntry[] = [{ time: "10:00:00", level: "info", msg: "a" }];
    const next: LogEntry[] = [{ time: "10:00:00", level: "info", msg: "" }];
    expect(logsTailChanged(prev, next)).toBe(true);
  });
});
