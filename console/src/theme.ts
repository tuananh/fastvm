/** Fully resolved theme (no `auto`); mirrors the `data-theme` attribute value. */
export type ResolvedThemeMode = "dark" | "light";

/**
 * Semantic palette source used by the terminal renderer.
 */
export interface ThemePalette {
  readonly mode: ResolvedThemeMode;
  /** Interactive accent: links, primary buttons, focus rings, selected items. */
  readonly accent: string;
  /** Brighter accent for hover states and decorative glows. */
  readonly accentHover: string;
  /** Text/icon color on top of `accent`. */
  readonly accentOn: string;
  readonly bgBase: string;
  readonly bgLayout: string;
  readonly bgContainer: string;
  readonly bgElevated: string;
  /** Drawer panel surface (slightly darker than elevated in dark mode). */
  readonly bgDrawer: string;
  readonly bgHeader: string;
  /** Sunken surfaces: table heads, metric chips, subtle panels. */
  readonly bgSubtle: string;
  readonly border: string;
  readonly borderSecondary: string;
  readonly borderStrong: string;
  readonly text: string;
  readonly textSecondary: string;
  readonly textTertiary: string;
  readonly textMuted: string;
  readonly success: string;
  readonly warning: string;
  readonly error: string;
  readonly info: string;
  readonly purple: string;
  readonly menuHoverBg: string;
  readonly menuSelectedBg: string;
  readonly rowHoverBg: string;
  readonly selectSelectedBg: string;
  readonly tagBg: string;
  readonly tagColor: string;
  readonly progressRemaining: string;
  readonly tooltipBg: string;
  readonly tooltipText: string;
  readonly mask: string;
}

/** Dark palette — keeps the console's original mint-on-charcoal identity. */
export const darkPalette: ThemePalette = {
  mode: "dark",
  accent: "#818cf8",
  accentHover: "#a5b4fc",
  accentOn: "#eef2ff",
  bgBase: "#030712",
  bgLayout: "#030712",
  bgContainer: "#111827",
  bgElevated: "#111827",
  bgDrawer: "#111827",
  bgHeader: "#111827",
  bgSubtle: "#1f2937",
  border: "#1f2937",
  borderSecondary: "#1f2937",
  borderStrong: "#374151",
  text: "#f9fafb",
  textSecondary: "#9ca3af",
  textTertiary: "#6b7280",
  textMuted: "#9ca3af",
  success: "#22c55e",
  warning: "#f59e0b",
  error: "#ef4444",
  info: "#3b82f6",
  purple: "#a855f7",
  menuHoverBg: "rgba(94, 234, 212, 0.10)",
  menuSelectedBg: "rgba(94, 234, 212, 0.16)",
  rowHoverBg: "rgba(94, 234, 212, 0.06)",
  selectSelectedBg: "rgba(94, 234, 212, 0.14)",
  tagBg: "#1f2937",
  tagColor: "#9ca3af",
  progressRemaining: "rgba(148, 163, 184, 0.18)",
  tooltipBg: "#1f2937",
  tooltipText: "#f9fafb",
  mask: "rgba(3, 6, 12, 0.65)",
};

/**
 * Light palette — cool near-white surfaces with a deep teal accent that keeps
 * white button text at WCAG AA (4.9:1 on #0b7f70) and dark body copy at ~15:1.
 */
export const lightPalette: ThemePalette = {
  mode: "light",
  accent: "#4f46e5",
  accentHover: "#4338ca",
  accentOn: "#ffffff",
  bgBase: "#ffffff",
  bgLayout: "#f6f7fb",
  bgContainer: "#ffffff",
  bgElevated: "#ffffff",
  bgDrawer: "#ffffff",
  bgHeader: "#ffffff",
  bgSubtle: "#f3f4f6",
  border: "#e5e7eb",
  borderSecondary: "#e5e7eb",
  borderStrong: "#d1d5db",
  text: "#111827",
  textSecondary: "#4b5563",
  textTertiary: "#6b7280",
  textMuted: "#4b5563",
  success: "#15803d",
  warning: "#b45309",
  error: "#d92d20",
  info: "#1d4ed8",
  purple: "#7c3aed",
  menuHoverBg: "rgba(15, 157, 138, 0.10)",
  menuSelectedBg: "rgba(15, 157, 138, 0.14)",
  rowHoverBg: "rgba(15, 157, 138, 0.06)",
  selectSelectedBg: "rgba(15, 157, 138, 0.12)",
  tagBg: "#f3f4f6",
  tagColor: "#4b5563",
  progressRemaining: "rgba(15, 23, 42, 0.08)",
  tooltipBg: "#1f2937",
  tooltipText: "#ffffff",
  mask: "rgba(17, 24, 39, 0.35)",
};

/** ANSI terminal palette passed to ghostty-web (`ITheme`-compatible shape). */
export interface TerminalTheme {
  readonly background: string;
  readonly foreground: string;
  readonly cursor: string;
  readonly cursorAccent: string;
  readonly selectionBackground: string;
  readonly selectionForeground: string;
  readonly black: string;
  readonly red: string;
  readonly green: string;
  readonly yellow: string;
  readonly blue: string;
  readonly magenta: string;
  readonly cyan: string;
  readonly white: string;
  readonly brightBlack: string;
  readonly brightRed: string;
  readonly brightGreen: string;
  readonly brightYellow: string;
  readonly brightBlue: string;
  readonly brightMagenta: string;
  readonly brightCyan: string;
  readonly brightWhite: string;
}

/**
 * Terminal colors for a resolved theme. ANSI colors are tuned per mode so both
 * normal and bright variants stay legible on the terminal background (the light
 * theme intentionally darkens "white" to a readable grey).
 */
export function terminalTheme(mode: ResolvedThemeMode): TerminalTheme {
  const palette = mode === "light" ? lightPalette : darkPalette;
  if (mode === "light") {
    return {
      background: "#ffffff",
      foreground: palette.text,
      cursor: palette.accent,
      cursorAccent: palette.accentOn,
      selectionBackground: "rgba(79, 70, 229, 0.20)",
      selectionForeground: "#111827",
      black: "#111827",
      red: "#b91c1c",
      green: "#15803d",
      yellow: "#a16207",
      blue: "#1d4ed8",
      magenta: "#7e22ce",
      cyan: "#0e7490",
      white: "#4b5563",
      brightBlack: "#6b7280",
      brightRed: "#dc2626",
      brightGreen: "#16a34a",
      brightYellow: "#ca8a04",
      brightBlue: "#2563eb",
      brightMagenta: "#9333ea",
      brightCyan: "#0891b2",
      brightWhite: "#111827",
    };
  }
  return {
    background: "#030712",
    foreground: palette.text,
    cursor: palette.accent,
    cursorAccent: palette.accentOn,
    selectionBackground: "rgba(129, 140, 248, 0.30)",
    selectionForeground: "#f9fafb",
    black: "#1f2937",
    red: "#f87171",
    green: "#4ade80",
    yellow: "#fbbf24",
    blue: "#60a5fa",
    magenta: "#c084fc",
    cyan: "#22d3ee",
    white: "#d1d5db",
    brightBlack: "#6b7280",
    brightRed: "#fca5a5",
    brightGreen: "#86efac",
    brightYellow: "#fcd34d",
    brightBlue: "#93c5fd",
    brightMagenta: "#d8b4fe",
    brightCyan: "#67e8f9",
    brightWhite: "#f9fafb",
  };
}

/** Dark accent kept as a named export for callers that need the brand mint. */
export const ACCENT = darkPalette.accent;

export const fontFamily =
  '"Inter", ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif';
export const monoFontFamily =
  '"JetBrains Mono", ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace';
