import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { credential, lastCall, pending, problem, watch } from "@/test/watches";

const kindCard = (name: string) => screen.getByRole("radio", { name: new RegExp(name) });

describe("WatchFormPage (new)", () => {
  // 001-AC4: a pasted URL picks the kind and fills its fields and a name.
  it("pre-fills kind, config and name from a pasted URL", async () => {
    mockApi({ "/api/v1/credentials": { items: [] } });
    renderApp("/watches/new");
    await userEvent.type(
      screen.getByLabelText(/Paste a URL/),
      "https://github.com/golang/go/pull/42",
    );
    expect(kindCard("GitHub pull request")).toBeChecked();
    expect(screen.getByLabelText("Repository")).toHaveValue("golang/go");
    expect(screen.getByLabelText("Pull request number")).toHaveValue(42);
    expect(screen.getByLabelText("Name")).toHaveValue("golang/go #42");
    expect(screen.getByText(/Recognized: GitHub pull request/)).toBeInTheDocument();

    await userEvent.clear(screen.getByLabelText(/Paste a URL/));
    await userEvent.type(screen.getByLabelText(/Paste a URL/), "nonsense");
    expect(screen.getByText(/Not recognized/)).toBeInTheDocument();
  });

  // 008-AC15: both boxes share one hint on the extra request, and reach the config.
  it("offers the new-items boxes with the request hint", async () => {
    const fn = mockApi({
      "/api/v1/credentials": { items: [] },
      "/api/v1/watches": () => problem(400, "x"),
    });
    renderApp("/watches/new");
    const issues = screen.getByRole("checkbox", { name: "Report new issues" });
    const prs = screen.getByRole("checkbox", { name: "Report new pull requests" });
    for (const box of [issues, prs]) {
      expect(box).not.toBeChecked();
      expect(box).toHaveAccessibleDescription(/1 more request:.*60 an hour without a token/);
    }
    expect(screen.getAllByText(/1 more request:/)).toHaveLength(1);
    expect(screen.getByRole("checkbox", { name: "Include pre-releases" })).not.toHaveAttribute(
      "aria-describedby",
    );
    await userEvent.type(screen.getByLabelText("Repository"), "a/b");
    await userEvent.click(issues);
    await userEvent.click(prs);
    await userEvent.click(screen.getByRole("button", { name: "Create watch" }));
    await waitFor(() => expect(lastCall(fn, "/api/v1/watches", "POST")).toBeDefined());
    expect(lastCall(fn, "/api/v1/watches", "POST")?.body?.config).toMatchObject({
      new_issues: true,
      new_prs: true,
    });
    await userEvent.click(kindCard("GitHub pull request"));
    expect(screen.queryByRole("checkbox", { name: "Report new issues" })).not.toBeInTheDocument();
  });

  // 011-AC17: the discussions box has its own hint on the extra requests, and reaches the config.
  it("offers the new discussions box with its request hint", async () => {
    const fn = mockApi({
      "/api/v1/credentials": { items: [] },
      "/api/v1/watches": () => problem(400, "x"),
    });
    renderApp("/watches/new");
    const box = screen.getByRole("checkbox", { name: "Report new discussions" });
    expect(box).not.toBeChecked();
    expect(box).toHaveAccessibleDescription(/2 to 3 more requests.*60 an hour without a token/);
    expect(
      screen.getByRole("checkbox", { name: "Report new pull requests" }),
    ).not.toHaveAccessibleDescription(/2 to 3/);
    await userEvent.type(screen.getByLabelText("Repository"), "a/b");
    await userEvent.click(box);
    await userEvent.click(screen.getByRole("button", { name: "Create watch" }));
    await waitFor(() => expect(lastCall(fn, "/api/v1/watches", "POST")).toBeDefined());
    expect(lastCall(fn, "/api/v1/watches", "POST")?.body?.config).toMatchObject({
      new_issues: false,
      new_discussions: true,
    });
  });

  // 008-AC13: an issue URL picks the GitHub issue card and fills its fields.
  it("picks GitHub issue from a pasted issue URL", async () => {
    const fn = mockApi({
      "/api/v1/credentials": { items: [] },
      "/api/v1/watches": () => problem(400, "x"),
    });
    renderApp("/watches/new");
    await userEvent.type(screen.getByLabelText(/Paste a URL/), "https://github.com/o/r/issues/12");
    expect(kindCard("GitHub issue")).toBeChecked();
    expect(screen.getByText(/State, labels and comments of one issue/)).toBeInTheDocument();
    expect(screen.getByLabelText("Repository")).toHaveValue("o/r");
    expect(screen.getByLabelText("Issue number")).toHaveValue(12);
    expect(screen.getByLabelText("Name")).toHaveValue("o/r #12");
    await userEvent.click(screen.getByRole("button", { name: "Create watch" }));
    await waitFor(() => expect(lastCall(fn, "/api/v1/watches", "POST")).toBeDefined());
    expect(lastCall(fn, "/api/v1/watches", "POST")?.body).toMatchObject({
      kind: "github_issue",
      config: { repo: "o/r", number: 12 },
    });
  });

  // 011-AC10: a discussion URL picks the GitHub discussion card and fills its fields.
  it("picks GitHub discussion from a pasted discussion URL", async () => {
    const fn = mockApi({
      "/api/v1/credentials": { items: [] },
      "/api/v1/watches": () => problem(400, "x"),
    });
    renderApp("/watches/new");
    expect(
      screen.getByText(/repository, release, issue, discussion, PR or MR page/),
    ).toBeInTheDocument();
    await userEvent.type(
      screen.getByLabelText(/Paste a URL/),
      "https://github.com/o/r/discussions/12",
    );
    expect(kindCard("GitHub discussion")).toBeChecked();
    expect(
      screen.getByText(/State, answer, labels and comments of one discussion/),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Repository")).toHaveValue("o/r");
    expect(screen.getByLabelText("Discussion number")).toHaveValue(12);
    expect(screen.getByLabelText("Name")).toHaveValue("o/r #12");
    await userEvent.click(screen.getByRole("button", { name: "Create watch" }));
    await waitFor(() => expect(lastCall(fn, "/api/v1/watches", "POST")).toBeDefined());
    expect(lastCall(fn, "/api/v1/watches", "POST")?.body).toMatchObject({
      kind: "github_discussion",
      config: { repo: "o/r", number: 12 },
    });
  });

  it("keeps compatible fields when the kind changes", async () => {
    mockApi({ "/api/v1/credentials": { items: [] } });
    renderApp("/watches/new");
    await userEvent.type(screen.getByLabelText("Repository"), "golang/go");
    await userEvent.click(kindCard("GitHub pull request"));
    expect(screen.getByLabelText("Repository")).toHaveValue("golang/go");
    await userEvent.click(kindCard("GitLab releases"));
    expect(screen.queryByLabelText("Repository")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Project path")).toHaveValue("");
    await userEvent.click(kindCard("GitHub repository"));
    expect(screen.getByLabelText("Repository")).toHaveValue("golang/go");
  });

  it("offers schedule presets with a readable preview", async () => {
    mockApi({ "/api/v1/credentials": { items: [] } });
    renderApp("/watches/new");
    await userEvent.click(screen.getByRole("button", { name: "Daily 09:00" }));
    expect(screen.getByLabelText("Schedule")).toHaveValue("0 9 * * *");
    expect(screen.getByText("Every day at 09:00")).toBeInTheDocument();
    await userEvent.clear(screen.getByLabelText("Schedule"));
    await userEvent.type(screen.getByLabelText("Schedule"), "0 9 1 * 1");
    expect(screen.getByText(/A 5-field cron expression/)).toBeInTheDocument();
  });

  // 001-AC4: the 400's field errors show under their fields.
  it("shows field-level errors from the server", async () => {
    mockApi({
      "/api/v1/credentials": { items: [] },
      "/api/v1/watches": () =>
        problem(400, "invalid watch", [
          { location: "body.config.repo", message: 'expected "owner/name"' },
          { location: "body.schedule", message: "invalid cron expression" },
        ]),
    });
    renderApp("/watches/new");
    await userEvent.type(screen.getByLabelText("Repository"), "nope");
    await userEvent.click(screen.getByRole("button", { name: "Create watch" }));
    expect(await screen.findByText('expected "owner/name"')).toBeInTheDocument();
    expect(screen.getByLabelText("Repository")).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByLabelText("Schedule")).toHaveAccessibleDescription(
      /invalid cron expression/,
    );
    expect(screen.getByRole("status")).toHaveTextContent("Could not save the watch: invalid watch");
  });

  // Errors on fields the form does not render (a checkbox, a hidden credential) reach the top alert.
  it("shows errors that no field renders at the top of the form", async () => {
    mockApi({
      "/api/v1/credentials": { items: [] },
      "/api/v1/watches": () =>
        problem(400, "invalid watch", [
          { location: "body.config.include_prereleases", message: "expected true or false" },
          { location: "body.config.repo", message: "bad repo" },
        ]),
    });
    renderApp("/watches/new");
    await userEvent.click(screen.getByRole("button", { name: "Create watch" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("include_prereleases: expected true or false");
    expect(alert).not.toHaveTextContent("bad repo");
    expect(screen.getByText("bad repo")).toBeInTheDocument();
  });

  it("disables submit while saving, then opens the new watch", async () => {
    let release: (r: Response) => void = () => {};
    const fn = mockApi({
      "/api/v1/credentials": { items: [credential()] },
      "/api/v1/watches": () => new Promise<Response>((r) => (release = r)),
      "/api/v1/watches/w9": watch({ id: "w9", name: "Mine" }),
    });
    renderApp("/watches/new");
    await userEvent.selectOptions(
      await screen.findByLabelText("Credential (optional)"),
      await screen.findByRole("option", { name: "GitHub bot (••••cdef)" }),
    );
    expect(lastCall(fn, "/api/v1/credentials")?.url.searchParams.get("kind")).toBe("github_token");
    await userEvent.type(screen.getByLabelText("Name"), "Mine");
    await userEvent.type(screen.getByLabelText("Repository"), "a/b");
    await userEvent.click(screen.getByRole("button", { name: "Create watch" }));
    expect(screen.getByRole("button", { name: "Saving…" })).toBeDisabled();
    expect(lastCall(fn, "/api/v1/watches", "POST")?.body).toEqual({
      name: "Mine",
      kind: "github_repo",
      config: {
        repo: "a/b",
        include_prereleases: false,
        new_issues: false,
        new_prs: false,
        new_discussions: false,
      },
      credential_id: "c1",
    });
    release(Response.json(watch({ id: "w9", name: "Mine" }), { status: 201 }));
    expect(await screen.findByRole("heading", { level: 1, name: "Mine" })).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Created Mine");
  });

  it("names the host a selected credential is sent to", async () => {
    mockApi({ "/api/v1/credentials": { items: [credential()] } });
    renderApp("/watches/new");
    const select = await screen.findByLabelText("Credential (optional)");
    expect(screen.queryByText(/is sent to/)).not.toBeInTheDocument();
    await userEvent.selectOptions(
      select,
      await screen.findByRole("option", { name: /GitHub bot/ }),
    );
    expect(select).toHaveAccessibleDescription(/Token is sent to api\.github\.com/);
    await userEvent.type(
      screen.getByLabelText("API URL (optional)"),
      "https://ghe.example.com/api/v3",
    );
    expect(select).toHaveAccessibleDescription(/Token is sent to ghe\.example\.com/);
  });

  it("drops a credential of another family when the kind changes", async () => {
    const fn = mockApi({
      "/api/v1/credentials": { items: [credential()] },
      "/api/v1/watches": () => problem(400, "x"),
    });
    renderApp("/watches/new");
    await userEvent.selectOptions(
      await screen.findByLabelText("Credential (optional)"),
      await screen.findByRole("option", { name: /GitHub bot/ }),
    );
    await userEvent.click(kindCard("Image tags"));
    await userEvent.click(screen.getByRole("button", { name: "Create watch" }));
    await waitFor(() => expect(lastCall(fn, "/api/v1/watches", "POST")).toBeDefined());
    expect(lastCall(fn, "/api/v1/watches", "POST")?.body?.credential_id).toBeUndefined();
  });
});

describe("WatchFormPage (edit)", () => {
  // 009-AC10: no read-only branch: the config and the credential stay editable.
  it("edits name and config; the kind is locked", async () => {
    let saved = false;
    const fn = mockApi({
      "/api/v1/credentials": { items: [credential()] },
      "/api/v1/watches/w1": (_: URL, init?: RequestInit) => {
        saved ||= init?.method === "PATCH";
        return Response.json(saved ? watch({ name: "Renamed" }) : watch({ credential_id: "c1" }));
      },
    });
    renderApp("/watches/w1/edit");
    const name = await screen.findByLabelText("Name");
    expect(name).toHaveValue("Go releases");
    expect(kindCard("GitHub repository")).toBeDisabled();
    expect(screen.queryByLabelText(/Paste a URL/)).not.toBeInTheDocument();
    await userEvent.clear(name);
    await userEvent.type(name, "Renamed");
    await screen.findByRole("option", { name: "GitHub bot (••••cdef)" });
    expect(screen.getByLabelText("Credential (optional)")).toHaveValue("c1");
    await userEvent.clear(screen.getByLabelText("Repository"));
    await userEvent.type(screen.getByLabelText("Repository"), "golang/tools");
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Renamed" })).toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/watches/w1", "PATCH")?.body).toEqual({
      name: "Renamed",
      config: {
        repo: "golang/tools",
        include_prereleases: false,
        new_issues: false,
        new_prs: false,
        new_discussions: false,
        api_url: "https://api.github.com",
      },
      schedule: "15m",
      credential_id: "c1",
    });
  });

  it("shows a missing watch as not found and reports load errors", async () => {
    mockApi({ "/api/v1/watches/w2": () => problem(404, "watch not found") });
    const second = renderApp("/watches/w2/edit");
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeInTheDocument();
    second.unmount();

    mockApi({ "/api/v1/watches/w3": () => new Response("", { status: 500 }) });
    renderApp("/watches/w3/edit");
    expect(await screen.findByRole("alert")).toHaveTextContent("The watch could not be loaded.");
  });

  it("shows a loading state", () => {
    mockApi({ "/api/v1/watches/w1": pending });
    renderApp("/watches/w1/edit");
    expect(screen.getByText("Loading…")).toBeInTheDocument();
  });
});

describe("WatchFormPage default schedule", () => {
  // Unauthenticated GitHub allows 60 requests an hour: GitHub kinds start hourly.
  it("follows the kind until the user picks one", async () => {
    mockApi({ "/api/v1/credentials": { items: [] } });
    renderApp("/watches/new");
    const schedule = await screen.findByLabelText("Schedule", { exact: true });
    expect(schedule).toHaveValue("1h");
    await userEvent.click(kindCard("GitLab releases"));
    expect(schedule).toHaveValue("15m");
    await userEvent.click(kindCard("GitHub pull request"));
    expect(schedule).toHaveValue("1h");
    await userEvent.click(screen.getByRole("button", { name: "6 hours" }));
    await userEvent.click(kindCard("Image tags"));
    expect(schedule).toHaveValue("6h");
  });

  it("follows a kind set by a pasted URL", async () => {
    mockApi({ "/api/v1/credentials": { items: [] } });
    renderApp("/watches/new");
    await userEvent.type(
      screen.getByLabelText(/Paste a URL/),
      "https://gitlab.com/gitlab-org/gitlab/-/merge_requests/7",
    );
    expect(kindCard("GitLab merge request")).toBeChecked();
    expect(screen.getByLabelText("Schedule", { exact: true })).toHaveValue("15m");
  });
});
