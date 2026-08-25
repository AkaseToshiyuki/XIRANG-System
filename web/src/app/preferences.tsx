import { createContext, ReactNode, useContext, useEffect, useMemo, useState } from "react";

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
  const [language, setLanguage] = useState<Language>(() => localStorage.getItem("xirang.language") === "en" ? "en" : "zh");
  const [theme, setTheme] = useState<Theme>(() => localStorage.getItem("xirang.theme") === "light" ? "light" : "dark");
  const [refreshSeconds, setRefreshSecondsState] = useState(() => {
    const value = Number(localStorage.getItem("xirang.refreshSeconds"));
    return Number.isFinite(value) && value >= 2 && value <= 60 ? value : 10;
  });

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    document.documentElement.lang = language === "zh" ? "zh-CN" : "en";
    localStorage.setItem("xirang.theme", theme);
    localStorage.setItem("xirang.language", language);
  }, [language, theme]);

  function setRefreshSeconds(value: number) {
    const bounded = Math.min(60, Math.max(2, Math.round(value)));
    setRefreshSecondsState(bounded);
    localStorage.setItem("xirang.refreshSeconds", String(bounded));
  }

  const value = useMemo<Preferences>(() => ({
    language, setLanguage, theme, setTheme, refreshSeconds, setRefreshSeconds,
    refreshMs: refreshSeconds * 1000,
    t: (zh, en) => language === "zh" ? zh : en,
  }), [language, theme, refreshSeconds]);

  return <PreferencesContext.Provider value={value}>{children}</PreferencesContext.Provider>;
}

export function usePreferences(): Preferences {
  const value = useContext(PreferencesContext);
  if (!value) throw new Error("PreferencesProvider is required");
  return value;
}
