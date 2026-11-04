import { describe, expect, it } from "vitest";

import { ApiError } from "@/api/client";
import { fieldErrors, problemText, unplaced } from "@/lib/problems";

describe("fieldErrors", () => {
  // 001-AC4: the 400 problem's field locations map onto the form's fields.
  it("maps body and config locations to field names", () => {
    const err = new ApiError(400, "x", "invalid watch", [
      { location: "body.config.repo", message: "expected owner/name" },
      { location: "body.schedule", message: "invalid cron" },
      { location: "body.config.repo", message: "second message ignored" },
      { location: "body", message: "expected required property name to be present" },
      { location: "query.kind", message: "bad" },
    ]);
    expect(fieldErrors(err)).toEqual({
      repo: "expected owner/name",
      schedule: "invalid cron",
      name: "required",
      "": "bad",
    });
  });

  it("falls back to the detail, and ignores non-API errors", () => {
    expect(fieldErrors(new ApiError(409, "x", "already shared"))).toEqual({ "": "already shared" });
    expect(fieldErrors(new ApiError(500, "x"))).toEqual({});
    expect(fieldErrors(new Error("boom"))).toEqual({});
  });

  it("problemText prefers the detail", () => {
    expect(problemText(new ApiError(409, "p: HTTP 409", "in use"))).toBe("in use");
    expect(problemText(new ApiError(500, "p: HTTP 500"))).toBe("p: HTTP 500");
    expect(problemText(new Error("e"))).toBe("e");
    expect(problemText("s")).toBe("s");
  });
});

describe("unplaced", () => {
  it("keeps the errors no field shows, form-level first as given", () => {
    expect(
      unplaced({ "": "top", name: "blank", include_prereleases: "expected true or false" }, [
        "name",
      ]),
    ).toEqual(["top", "include_prereleases: expected true or false"]);
    expect(unplaced({ name: "blank" }, ["name"])).toEqual([]);
  });
});
