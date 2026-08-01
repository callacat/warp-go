import { describe, expect, it } from "vitest";
import { extractEventData, extractIsDark } from "./useTheme";

describe("extractEventData", () => {
  it("unwraps the data field of a WailsEvent-shaped argument", () => {
    expect(
      extractEventData({
        name: "android:ThemeChanged",
        data: '{"isDarkMode":true}',
      }),
    ).toBe('{"isDarkMode":true}');
  });

  it("passes through bare payloads unchanged", () => {
    expect(extractEventData('{"isDarkMode":false}')).toBe('{"isDarkMode":false}');
    expect(extractEventData({ isDarkMode: true })).toEqual({ isDarkMode: true });
    expect(extractEventData(null)).toBeNull();
    expect(extractEventData(undefined)).toBeUndefined();
  });
});

describe("extractIsDark", () => {
  it("reads a boolean isDarkMode field from an object payload", () => {
    expect(extractIsDark({ isDarkMode: true })).toBe(true);
    expect(extractIsDark({ isDarkMode: false })).toBe(false);
  });

  it("parses Android JSON-string payloads", () => {
    expect(extractIsDark('{"isDarkMode":true}')).toBe(true);
    expect(extractIsDark('{"isDarkMode":false}')).toBe(false);
  });

  it("returns null when no usable isDarkMode is present", () => {
    expect(extractIsDark(null)).toBeNull();
    expect(extractIsDark(undefined)).toBeNull();
    expect(extractIsDark(42)).toBeNull();
    expect(extractIsDark("not json")).toBeNull();
    expect(extractIsDark('{"foo":1}')).toBeNull();
    expect(extractIsDark({ foo: 1 })).toBeNull();
    expect(extractIsDark({ isDarkMode: "true" })).toBeNull();
  });
});
