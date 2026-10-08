import { createContext, ReactNode, useCallback, useContext, useEffect, useMemo, useState } from "react";

export type Language = "zh" | "en";
export type Theme = "dark" | "light";

interface Preferences {
  language: Language;
  setLanguage: (value: Language) => void;
  theme: Theme;
  setTheme: (value: Theme) => void;
  refreshSeconds: number;
  setRefreshSeconds: (value: number) => void;
  refreshMs: number;
  t: (zh: string, en: string) => string;
}

const PreferencesContext = createContext<Preferences | null>(null);

export function PreferencesProvider({ children }: { children: ReactNode }) {
  const [language, setLanguage] = useState<Language>(() => readPreference("xirang.language") === "en" ? "en" : "zh");
  const [theme, setTheme] = useState<Theme>(() => readPreference("xirang.theme") === "light" ? "light" : "dark");
  const [refreshSeconds, setRefreshSecondsState] = useState(() => {
    const value = Number(readPreference("xirang.refreshSeconds"));
    return Number.isFinite(value) && value >= 2 && value <= 60 ? value : 10;
  });

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    document.documentElement.lang = language === "zh" ? "zh-CN" : "en";
    writePreference("xirang.theme", theme);
    writePreference("xirang.language", language);
  }, [language, theme]);

  const setRefreshSeconds = useCallback((value: number) => {
    const bounded = Number.isFinite(value) ? Math.min(60, Math.max(2, Math.round(value))) : 10;
    setRefreshSecondsState(bounded);
    writePreference("xirang.refreshSeconds", String(bounded));
  }, []);

  const t = useCallback((zh: string, en: string) => language === "zh" ? zh : en, [language]);

  const value = useMemo<Preferences>(() => ({
    language, setLanguage, theme, setTheme, refreshSeconds, setRefreshSeconds,
    refreshMs: refreshSeconds * 1000,
    t,
  }), [language, theme, refreshSeconds, setRefreshSeconds, t]);

  return <PreferencesContext.Provider value={value}>{children}</PreferencesContext.Provider>;
}

export function usePreferences(): Preferences {
  const value = useContext(PreferencesContext);
  if (!value) throw new Error("PreferencesProvider is required");
  return value;
}

function readPreference(key: string): string | null {
  try { return localStorage.getItem(key); } catch { return null; }
}

function writePreference(key: string, value: string) {
  try { localStorage.setItem(key, value); } catch { /* Preferences remain available for this session. */ }
}
