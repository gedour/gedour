import { describe, expect, it } from "vitest";

import { ApiError } from "@/api/client";
import { filterProblem, insertAt, marker } from "@/lib/filter";

const problem = (value: unknown, location = "body.filter") =>
  new ApiError(400, "x", "invalid filter", [
    { location, message: "unexpected token (line 2, column 3)", value },
  ]);

describe("filter helpers", () => {
  // 001-AC13: the position travels in the problem's value.
  it("reads the filter error and its position out of a 400", () => {
    expect(filterProblem(problem({ line: 2, column: 3 }))).toEqual({
      message: "unexpected token (line 2, column 3)",
      line: 2,
      column: 3,
    });
    expect(filterProblem(problem(null))).toEqual({
      message: "unexpected token (line 2, column 3)",
    });
    expect(filterProblem(problem({ line: 1 }, "body.name"))).toBeNull();
    expect(filterProblem(new Error("boom"))).toBeNull();
  });

  it("splits the faulty line around the error column", () => {
    const src = 'event.type == "x" &&\n  watch.nme';
    expect(marker(src, { message: "", line: 2, column: 9 })).toEqual({
      line: 2,
      before: "  watch.",
      at: "n",
      after: "me",
    });
    // At end of input the marker sits after the last character.
    expect(marker("event.type ==", { message: "", line: 1, column: 14 })).toEqual({
      line: 1,
      before: "event.type ==",
      at: "",
      after: "",
    });
    expect(marker(src, { message: "", line: 5, column: 1 })).toBeNull();
    expect(marker(src, { message: "" })).toBeNull();
  });

  it("inserts over the selection", () => {
    expect(insertAt("a  b", 2, 2, "x")).toEqual({ value: "a x b", caret: 3 });
    expect(insertAt("abc", 0, 3, "watch.name")).toEqual({ value: "watch.name", caret: 10 });
  });
});
