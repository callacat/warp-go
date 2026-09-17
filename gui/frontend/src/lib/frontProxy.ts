/**
 * 百度中转（front proxy）启用状态的纯函数库。
 *
 * 为什么单独成库：Settings 页的开关要联动 Scan 页置灰（启用后忽略边缘优选
 * IP——CONNECT 目标被改写成 CF 优选裸 IP 会 503），而两页没有共同父级状态，
 * 判定逻辑与提示文案集中在这里，便于单测（与 theme.ts 同风格：纯函数进单测，
 * DOM/React 部分留在 hook 与 context 里）。
 */

/** 配置里 front proxy 是否启用（缺字段按未启用）。 */
export function frontProxyEnabled(
  config: { frontProxy?: { enabled?: boolean } } | null | undefined,
): boolean {
  return config?.frontProxy?.enabled ?? false;
}

/** 边缘扫描/应用按钮置灰时的说明（front proxy 与优选 IP 互斥）。 */
export const EDGE_PICKING_DISABLED_HINT =
  "已启用百度中转（保命通道）：该模式忽略边缘优选 IP，扫描与应用已停用。";
