/**
 * 百度中转启用状态的共享实例（App 根挂载，见 FrontProxyContext.tsx）。
 *
 * 与 useTheme 同一套路：挂载时读一次 config.json 拿到持久化的启用状态——
 * Scan 页必须在用户没打开过 Settings 页时也知道要不要置灰（否则冷启动直接
 * 进扫描页会给出「可扫描」的错误信号）；Settings 页的开关改动经
 * setEnabled 立即广播给所有消费方。
 *
 * 注意：这里只镜像 UI 状态，不写配置文件——落盘仍由 Settings 页的
 * 「保存配置」完成（与 useTheme 的 setMode 不同，开关本身不是即时生效项）。
 */

import { useCallback, useEffect, useState } from "react";
import { getConfig } from "./api";
import { frontProxyEnabled } from "./frontProxy";

export function useFrontProxy(initialConfig?: { frontProxy?: { enabled?: boolean } } | null): {
  enabled: boolean;
  setEnabled: (enabled: boolean) => void;
  setFromConfig: (config: { frontProxy?: { enabled?: boolean } } | null) => void;
} {
  const [enabled, setEnabledState] = useState<boolean>(() => frontProxyEnabled(initialConfig ?? null));

  // 挂载时读一次配置：冷启动直接进扫描页也要拿到真实的启用状态。
  useEffect(() => {
    getConfig()
      .then((c) => setEnabledState(frontProxyEnabled(c ?? null)))
      .catch(() => {
        /* bridge 不可用（纯浏览器 dev）——保持当前值 */
      });
  }, []);

  const setEnabled = useCallback((next: boolean) => {
    setEnabledState(next);
  }, []);

  const setFromConfig = useCallback((config: { frontProxy?: { enabled?: boolean } } | null) => {
    setEnabledState(frontProxyEnabled(config));
  }, []);

  return { enabled, setEnabled, setFromConfig };
}
