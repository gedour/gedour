import { CompletionContext } from "@codemirror/autocomplete";
import { syntaxTree } from "@codemirror/language";
import { forEachDiagnostic } from "@codemirror/lint";
import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { act, render, screen } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { ScriptEditor } from "@/components/ScriptEditor";
import { ThemeProvider } from "@/hooks/ThemeProvider";
import { useTheme } from "@/hooks/theme";
import type { FilterProblem } from "@/lib/filter";
import { dryRunEventOf, problemRange, scriptCompletions } from "@/lib/scripts";

function view(): EditorView {
  const content = document.querySelector(".cm-content");
  const v = content && EditorView.findFromDOM(content as HTMLElement);
  if (!v) throw new Error("no editor");
  return v;
}

function Harness({
  initial,
  problem = null,
  onChange = () => {},
  onSave,
}: {
  initial: string;
  problem?: FilterProblem | null;
  onChange?: (v: string) => void;
  onSave?: () => void;
}) {
  const [value, setValue] = useState(initial);
  const { setTheme } = useTheme();
  return (
    <>
      <ScriptEditor
        value={value}
        onChange={(v) => {
          setValue(v);
          onChange(v);
        }}
        problem={problem}
        label="Source"
        describedBy="hint"
        onSave={onSave}
      />
      <button onClick={() => setValue("replaced()")}>replace</button>
      <button onClick={() => setTheme("dark")}>dark</button>
      <button onClick={() => setTheme("light")}>light</button>
      <output data-testid="value">{value}</output>
    </>
  );
}

const renderEditor = (props: Parameters<typeof Harness>[0]) =>
  render(
    <ThemeProvider>
      <Harness {...props} />
    </ThemeProvider>,
  );

// 001-AC18: the script editor is CodeMirror with JavaScript highlighting.
describe("ScriptEditor", () => {
  it("highlights JS, syncs its value both ways and labels the textbox", () => {
    const onChange = vi.fn();
    renderEditor({ initial: "function main() { return 1 }", onChange });
    const box = screen.getByRole("textbox", { name: "Source" });
    expect(box).toHaveAttribute("aria-describedby", "hint");
    // The JavaScript parser tokenized the keyword.
    expect(document.querySelector(".cm-content")?.textContent).toBe("function main() { return 1 }");
    expect(syntaxTree(view().state).topNode.name).toBe("Script");
    expect(syntaxTree(view().state).resolve(3, 1).name).toBe("function");

    act(() => view().dispatch({ changes: { from: 0, insert: "// hi\n" } }));
    expect(onChange).toHaveBeenLastCalledWith("// hi\nfunction main() { return 1 }");
    expect(screen.getByTestId("value")).toHaveTextContent("// hi");

    act(() => screen.getByRole("button", { name: "replace" }).click());
    expect(view().state.doc.toString()).toBe("replaced()");
  });

  it("follows the app theme", () => {
    renderEditor({ initial: "x" });
    expect(screen.getByTestId("script-editor")).toHaveAttribute("data-theme", "light");
    expect(view().state.facet(EditorView.darkTheme)).toBe(false);
    act(() => screen.getByRole("button", { name: "dark" }).click());
    expect(screen.getByTestId("script-editor")).toHaveAttribute("data-theme", "dark");
    expect(view().state.facet(EditorView.darkTheme)).toBe(true);
    act(() => screen.getByRole("button", { name: "light" }).click());
    expect(view().state.facet(EditorView.darkTheme)).toBe(false);
  });

  it("marks the syntax error at its line and column", () => {
    renderEditor({
      initial: "function main() {\n  return 1 +\n}",
      problem: { message: "Unexpected token }", line: 3, column: 1 },
    });
    const found: { from: number; message: string }[] = [];
    forEachDiagnostic(view().state, (d) => found.push({ from: d.from, message: d.message }));
    expect(found).toEqual([{ from: 31, message: "Unexpected token }" }]);
    expect(screen.getByRole("textbox", { name: "Source" })).toHaveAttribute("aria-invalid", "true");
  });

  it("saves on Mod-S", () => {
    const onSave = vi.fn();
    renderEditor({ initial: "x", onSave });
    act(() => {
      view().contentDOM.dispatchEvent(
        new KeyboardEvent("keydown", { key: "s", ctrlKey: true, bubbles: true, cancelable: true }),
      );
    });
    expect(onSave).toHaveBeenCalled();
  });
});

describe("problemRange", () => {
  const state = EditorState.create({ doc: "ab\ncdef" });
  it("clamps into the document", () => {
    expect(problemRange(state, { message: "", line: 2, column: 3 })).toEqual({ from: 5, to: 6 });
    expect(problemRange(state, { message: "", line: 9, column: 99 })).toEqual({ from: 7, to: 7 });
    expect(problemRange(state, { message: "" })).toEqual({ from: 0, to: 1 });
  });
});

describe("scriptCompletions", () => {
  const at = (doc: string, explicit = false) =>
    scriptCompletions(new CompletionContext(EditorState.create({ doc }), doc.length, explicit));
  it("offers the script's globals for a dotted word", () => {
    const r = at("const t = event.ty");
    expect(r?.from).toBe(10);
    expect(r?.options.map((o) => o.label)).toContain("event.type");
    expect(r?.options.map((o) => o.label)).toContain("http.fetch");
    expect(at("x = ")).toBeNull();
    expect(at("x = ", true)?.options.length).toBeGreaterThan(5);
  });
});

describe("dryRunEventOf", () => {
  it("reads the picker's key", () => {
    expect(dryRunEventOf("event:7")).toEqual({ event_id: 7 });
    expect(dryRunEventOf("sample:1")).toMatchObject({ sample: { type: "tag.created" } });
    expect(dryRunEventOf("sample:99")).toMatchObject({ sample: { type: "release.published" } });
  });
});
