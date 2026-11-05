import js from "@eslint/js";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import globals from "globals";
import tseslint from "typescript-eslint";

const NO_NAV = "Navigate by script only in src/api/client.ts; show the sign-in gate instead.";
const noScriptNav = (name) => ({ name, message: NO_NAV });
// Any spelling of the API client module: alias or relative, "./" segments, extension, index file, query.
const CLIENT_MODULE = "(^|[/\\\\])client(\\.[cm]?[jt]sx?|/(index(\\.[cm]?[jt]sx?)?)?)?(\\?.*)?$";

export default tseslint.config(
  { ignores: ["dist", "coverage", "playwright", "playwright-report", "test-results"] },
  {
    files: ["**/*.{ts,tsx}"],
    extends: [js.configs.recommended, ...tseslint.configs.strict],
    languageOptions: { globals: { ...globals.browser, ...globals.node } },
    plugins: { "react-hooks": reactHooks, "react-refresh": reactRefresh },
    rules: {
      ...reactHooks.configs.recommended.rules,
      "react-refresh/only-export-components": ["warn", { allowConstantExport: true }],
    },
  },
  {
    // 002-AC3: only api/client.ts navigates by script (sign-out, reload), so a lost session can only ever
    // show the sign-in gate, never redirect. Tested by src/lint.test.ts. index.html's asset-recovery reload
    // is the one exception, outside these globs.
    files: ["src/**/*.{ts,tsx}"],
    ignores: ["src/api/client.ts", "src/**/*.test.{ts,tsx}", "src/test/**"],
    rules: {
      "no-restricted-globals": ["error", ...["location", "open", "navigation"].map(noScriptNav)],
      "no-restricted-imports": [
        "error",
        {
          patterns: [{ regex: CLIENT_MODULE, importNames: ["navigation"], message: NO_NAV }],
        },
      ],
      // A dynamic import would hand the whole module out, navigation included.
      "no-restricted-syntax": [
        "error",
        {
          selector: `ImportExpression[source.type='Literal'][source.value=/${CLIENT_MODULE.replaceAll("/", "\\/")}/]`,
          message: NO_NAV,
        },
        {
          selector: "ImportExpression:not([source.type='Literal'])",
          message: "Dynamic imports take a literal path, so the navigation rules can see them.",
        },
      ],
      "no-restricted-properties": [
        "error",
        ...["window", "self", "top", "parent", "frames", "globalThis", "document"].flatMap(
          (object) =>
            ["location", "open", "navigation"].map((property) => ({
              object,
              property,
              message: NO_NAV,
            })),
        ),
      ],
    },
  },
);
