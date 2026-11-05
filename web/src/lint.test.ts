import { globSync, readFileSync } from "node:fs";

import { ESLint } from "eslint";
import { describe, expect, it } from "vitest";

// vitest runs from web/, where eslint.config.js lives.
const eslint = new ESLint({ cwd: process.cwd() });
const NAV_RULES = new Set([
  "no-restricted-globals",
  "no-restricted-imports",
  "no-restricted-properties",
  "no-restricted-syntax",
]);

async function navErrors(code: string, filePath = "src/pages/Bad.tsx"): Promise<number> {
  const [result] = await eslint.lintText(code, { filePath });
  return (result?.messages ?? []).filter((m) => m.ruleId && NAV_RULES.has(m.ruleId)).length;
}

// 002-AC3: the lint rule is the guard that nothing but api/client.ts navigates by script.
describe("script navigation lint rule", () => {
  it.each([
    'window.location.assign("/auth/login");',
    "location.reload();",
    "window.location.reload();",
    'import { navigation } from "@/api/client";\nnavigation.reload();',
    'location.href = "/auth/login";',
    'window.location.replace(`/auth/login?return_to=${encodeURIComponent("/")}`);',
    "const { assign } = window.location;\nassign(`/auth/${'login'}`);",
    'const { location: l } = window;\nl.assign("/auth/login");',
    'document.location.replace("/auth/login");',
    'self["location"].assign("/auth/login");',
    'window.open("/auth/login");',
    'open("/auth/login");',
    'globalThis.navigation.navigate("/auth/login");',
    'import { navigation } from "@/api/client";\nnavigation.go("/auth/login");',
    'import { navigation as n } from "../api/client";\nn.go("/auth/login");',
    'import * as api from "@/api/client";\napi.navigation.go("/auth/login");',
    'export { navigation } from "@/api/client";',
    'import { navigation } from "@/api/client.ts";\nnavigation.go("/auth/login");',
    'import { navigation } from "@/api/./client";\nnavigation.go("/auth/login");',
    'import { navigation } from "./client.ts";\nnavigation.go("/auth/login");',
    'import { navigation } from "../api/client/index";\nnavigation.go("/auth/login");',
    'void import("@/api/client").then((m) => m.navigation.go("/auth/login"));',
    'void import("../api/client.ts").then((m) => m.navigation.go("/auth/login"));',
    "void import(`@/api/client`).then((m) => m.navigation.go('/auth/login'));",
    'const mod = "@/api/client";\nvoid import(mod);',
    'import { navigation } from "@/api/client?x";\nnavigation.go("/auth/login");',
    'void import("@/api/client?raw").then((m) => m.navigation.go("/auth/login"));',
  ])("flags %j outside the API client", async (code) => {
    expect(await navErrors(code)).toBeGreaterThan(0);
  });

  it.each([
    '<a href="/auth/login?return_to=%2F">Sign in</a>;',
    "const { pathname } = useLocation();\nconst dialog = { open: true };\ndialog.open = false;",
    'import { signOut } from "@/api/client";\nvoid signOut();',
    'import { reloadPage } from "@/api/client";\nreloadPage();',
    'import { signOut } from "@/api/client.ts";\nvoid signOut();',
    'void import("@/pages/ScriptFormPage");',
    'import { clientName } from "@/lib/clients";\nvoid clientName;',
  ])("lets %j through", async (code) => {
    expect(await navErrors(`declare function useLocation(): { pathname: string };\n${code}`)).toBe(
      0,
    );
  });

  it("allows the API client's own navigation", async () => {
    expect(await navErrors("window.location.assign(url);", "src/api/client.ts")).toBe(0);
  });

  it("lets tests use the navigation seam", async () => {
    const code = 'import { navigation } from "@/api/client";\nvi.spyOn(navigation, "go");';
    expect(await navErrors(code, "src/components/UserMenu.test.tsx")).toBe(0);
  });
});

// WCAG 1.4.11 asks 3:1 for a control; bg-line reads 1.2 to 1.4:1 on the surfaces a switch sits on.
describe("switch off state", () => {
  it("does not use bg-line", () => {
    const switches = globSync("src/**/*.tsx")
      .filter((file) => !file.endsWith(".test.tsx"))
      .flatMap((file) => {
        const source = readFileSync(file, "utf8");
        return [...source.matchAll(/role=\{?["'`]switch/g)].map((match) => ({
          file,
          element: source.slice(
            source.lastIndexOf("<", match.index),
            source.indexOf("</", match.index),
          ),
        }));
      });
    expect(switches.length).toBeGreaterThan(0);
    expect(
      switches.filter(({ element }) => /\bbg-line\b/.test(element)).map(({ file }) => file),
    ).toEqual([]);
  });
});
