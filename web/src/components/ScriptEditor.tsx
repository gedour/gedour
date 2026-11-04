import { autocompletion } from "@codemirror/autocomplete";
import { javascript, javascriptLanguage } from "@codemirror/lang-javascript";
import { HighlightStyle, syntaxHighlighting } from "@codemirror/language";
import { setDiagnostics } from "@codemirror/lint";
import { Compartment, type Extension } from "@codemirror/state";
import { EditorView, keymap } from "@codemirror/view";
import { tags } from "@lezer/highlight";
import { oneDarkHighlightStyle } from "@codemirror/theme-one-dark";
import { basicSetup } from "codemirror";
import { useEffect, useRef } from "react";

import { useTheme } from "@/hooks/theme";
import type { FilterProblem } from "@/lib/filter";
import { problemRange, scriptCompletions } from "@/lib/scripts";

// One Dark's comment grey falls under 4.5:1 on the active line; the later spec wins for its tag.
const darkHighlight = HighlightStyle.define([
  ...oneDarkHighlightStyle.specs,
  { tag: tags.comment, color: "var(--muted)", fontStyle: "italic" },
]);

// Colours come from the app's CSS variables, so both themes follow the palette; `dark` switches
// CodeMirror's own defaults (cursor, selection) and the highlight style.
function themeFor(mode: "light" | "dark"): Extension {
  const dark = mode === "dark";
  return [
    EditorView.theme(
      {
        "&": {
          backgroundColor: "var(--surface)",
          color: "var(--ink)",
          fontSize: "13px",
          height: "100%",
        },
        "&.cm-focused": { outline: "2px solid var(--beacon)", outlineOffset: "-1px" },
        ".cm-scroller": { fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace" },
        ".cm-content": { caretColor: "var(--ink)", padding: "10px 0" },
        ".cm-gutters": {
          backgroundColor: "var(--bg)",
          color: "var(--muted)",
          borderRight: "1px solid var(--line)",
        },
        ".cm-activeLine": { backgroundColor: dark ? "#ffffff08" : "#1a233308" },
        ".cm-activeLineGutter": { backgroundColor: "transparent", color: "var(--ink)" },
        ".cm-tooltip": {
          backgroundColor: "var(--surface)",
          color: "var(--ink)",
          border: "1px solid var(--line)",
        },
        // The default underline is a data: URI image, which the CSP (img-src 'self') blocks.
        ".cm-lintRange-error": {
          backgroundImage: "none",
          textDecoration: "underline wavy #e11d48",
          textDecorationSkipInk: "none",
          textUnderlineOffset: "3px",
        },
        ".cm-tooltip-autocomplete ul li[aria-selected]": {
          backgroundColor: "var(--beacon-soft)",
          color: "var(--ink)",
        },
      },
      { dark },
    ),
    // Light keeps basicSetup's fallback highlight style.
    dark ? syntaxHighlighting(darkHighlight) : [],
  ];
}

/** CodeMirror 6 JavaScript editor, controlled by value; problem underlines the syntax error. */
export function ScriptEditor({
  value,
  onChange,
  problem,
  label,
  describedBy,
  onSave,
}: {
  value: string;
  onChange: (value: string) => void;
  problem: FilterProblem | null;
  label: string;
  describedBy?: string;
  /** Mod-S inside the editor. */
  onSave?: () => void;
}) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  const theme = useRef(new Compartment());
  const attrs = useRef(new Compartment());
  const handlers = useRef({ onChange, onSave });
  const { resolved } = useTheme();

  useEffect(() => {
    handlers.current = { onChange, onSave };
  });

  // One view for the component's life; the effects below keep it in sync.
  useEffect(() => {
    const v = new EditorView({
      parent: host.current as HTMLDivElement,
      doc: value,
      extensions: [
        basicSetup,
        javascript(),
        javascriptLanguage.data.of({ autocomplete: scriptCompletions }),
        autocompletion({ activateOnTyping: true }),
        keymap.of([
          {
            key: "Mod-s",
            preventDefault: true,
            run: () => {
              handlers.current.onSave?.();
              return true;
            },
          },
        ]),
        EditorView.updateListener.of((u) => {
          if (u.docChanged) handlers.current.onChange(u.state.doc.toString());
        }),
        theme.current.of(themeFor(resolved)),
        attrs.current.of([]),
      ],
    });
    view.current = v;
    return () => {
      v.destroy();
      view.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- created once; value, theme and labels sync below
  }, []);

  useEffect(() => {
    const v = view.current;
    if (v && v.state.doc.toString() !== value) {
      v.dispatch({ changes: { from: 0, to: v.state.doc.length, insert: value } });
    }
  }, [value]);

  useEffect(() => {
    view.current?.dispatch({ effects: theme.current.reconfigure(themeFor(resolved)) });
  }, [resolved]);

  useEffect(() => {
    const a: Record<string, string> = { "aria-label": label };
    if (describedBy) a["aria-describedby"] = describedBy;
    if (problem) a["aria-invalid"] = "true";
    view.current?.dispatch({
      effects: attrs.current.reconfigure(EditorView.contentAttributes.of(a)),
    });
  }, [label, describedBy, problem]);

  useEffect(() => {
    const v = view.current;
    if (!v) return;
    const diagnostics = problem
      ? [
          {
            ...problemRange(v.state, problem),
            severity: "error" as const,
            message: problem.message,
          },
        ]
      : [];
    v.dispatch(setDiagnostics(v.state, diagnostics));
  }, [problem, value]);

  return (
    <div
      ref={host}
      data-testid="script-editor"
      data-theme={resolved}
      className="h-[26rem] overflow-hidden rounded-lg border border-line bg-surface md:h-[32rem] [&_.cm-editor]:h-full"
    />
  );
}
