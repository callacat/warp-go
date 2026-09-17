/**
 * Front proxy 启用状态的根级实例（与 ThemeProvider 同构）。
 *
 * 为什么必须挂在 App 根：Settings 页与 Scan 页是两个独立的页面组件，各建一份
 * useFrontProxy() 会分叉状态——设置页开了开关，扫描页却还认为可扫描（正是
 * C5「GUI 未置灰优选 IP 输入」的形态）。
 */

import { createContext, useContext, type ReactNode } from "react";
import { useFrontProxy } from "./useFrontProxy";

type FrontProxyContextValue = {
  enabled: boolean;
  setEnabled: (enabled: boolean) => void;
  setFromConfig: (config: { frontProxy?: { enabled?: boolean } } | null) => void;
};

const FrontProxyContext = createContext<FrontProxyContextValue | null>(null);

export function FrontProxyProvider({ children }: { children: ReactNode }) {
  const frontProxy = useFrontProxy();
  return <FrontProxyContext.Provider value={frontProxy}>{children}</FrontProxyContext.Provider>;
}

/** 访问 App 根的 front proxy 状态。必须在 <FrontProxyProvider> 内使用。 */
export function useFrontProxyContext(): FrontProxyContextValue {
  const ctx = useContext(FrontProxyContext);
  if (!ctx) {
    throw new Error("useFrontProxyContext 必须在 <FrontProxyProvider> 内使用");
  }
  return ctx;
}
