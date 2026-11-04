import { fileURLToPath } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

import { injectThemeKey } from "./src/lib/theme.ts";

export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    // Single source for the storage key read by the pre-paint script in index.html.
    { name: "gedour-theme-key", transformIndexHtml: injectThemeKey },
  ],
  build: {
    // No data: URIs, so the CSP can stay at 'self' for fonts and images.
    assetsInlineLimit: 0,
  },
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  server: {
    // Dev only: the Go server owns /api and /healthz.
    proxy: {
      "/api": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
    },
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
    restoreMocks: true,
    coverage: {
      provider: "v8",
      include: ["src/**/*.{ts,tsx}"],
      exclude: ["src/main.tsx", "src/test/**", "src/**/*.test.{ts,tsx}"],
      reporter: ["text", "json-summary", "html"],
      thresholds: { lines: 80, statements: 80, functions: 80, branches: 80 },
    },
  },
});
