import React from "react";
import ReactDOM from "react-dom/client";
import "@fontsource-variable/noto-sans-sc";
import { App } from "./app/App";
import { PreferencesProvider } from "./app/preferences";
import "./app/styles.css";

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
  <PreferencesProvider><App /></PreferencesProvider>
  </React.StrictMode>,
);
