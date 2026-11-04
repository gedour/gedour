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

  it("keeps compatible fields when the kind changes", async () => {
    mockApi({ "/api/v1/credentials": { items: [] } });
    renderApp("/watches/new");
    await userEvent.type(screen.getByLabelText("Repository"), "golang/go");
    await userEvent.click(kindCard("GitHub pull request"));
    expect(screen.getByLabelText("Repository")).toHaveValue("golang/go");
    await userEvent.click(kindCard("GitLab releases"));
    expect(screen.queryByLabelText("Repository")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Project path")).toHaveValue("");
    await userEvent.click(kindCard("GitHub releases"));
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
      "/api/v1/watches/w9/shares": { items: [] },
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
      kind: "github_release",
      config: { repo: "a/b", include_prereleases: false },
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
  it("edits name and config; the kind is locked", async () => {
    let saved = false;
    const fn = mockApi({
      "/api/v1/credentials": { items: [] },
      "/api/v1/watches/w1": (_: URL, init?: RequestInit) => {
        saved ||= init?.method === "PATCH";
        return Response.json(saved ? watch({ name: "Renamed" }) : watch());
      },
      "/api/v1/watches/w1/shares": { items: [] },
    });
    renderApp("/watches/w1/edit");
    const name = await screen.findByLabelText("Name");
    expect(name).toHaveValue("Go releases");
    expect(kindCard("GitHub releases")).toBeDisabled();
    expect(screen.queryByLabelText(/Paste a URL/)).not.toBeInTheDocument();
    await userEvent.clear(name);
    await userEvent.type(name, "Renamed");
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Renamed" })).toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/watches/w1", "PATCH")?.body).toEqual({
      name: "Renamed",
      config: { repo: "golang/go", include_prereleases: false, api_url: "https://api.github.com" },
      schedule: "15m",
      credential_id: "",
    });
  });

  // 001-AC5: a rw sharee edits without the credential field; ro gets no form.
  it("hides the credential from a read-write sharee", async () => {
    let name = "A";
    const fn = mockApi({
      "/api/v1/watches/w1": (_: URL, init?: RequestInit) => {
        if (init?.method === "PATCH") name = "B";
        return Response.json(watch({ permission: "rw", name }));
      },
      "/api/v1/watches/w1/shares": { items: [] },
    });
    renderApp("/watches/w1/edit");
    await screen.findByLabelText("Name");
    expect(screen.queryByLabelText("Credential (optional)")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await screen.findByRole("heading", { level: 1, name: "B" });
    expect(lastCall(fn, "/api/v1/watches/w1", "PATCH")?.body).not.toHaveProperty("credential_id");
    expect(lastCall(fn, "/api/v1/credentials")).toBeUndefined();
  });

  // 001-AC5: the owner's credential pins the target; a rw sharee keeps name and schedule only.
  it("locks the config of a credentialed watch for a read-write sharee", async () => {
    let name = "A";
    const fn = mockApi({
      "/api/v1/watches/w1": (_: URL, init?: RequestInit) => {
        if (init?.method === "PATCH") name = "B";
        return Response.json(
          watch({ permission: "rw", credential_id: "c1", owner_name: "Alice", name }),
        );
      },
      "/api/v1/watches/w1/shares": { items: [] },
    });
    renderApp("/watches/w1/edit");
    const repo = await screen.findByLabelText("Repository");
    expect(repo).toHaveAttribute("readonly");
    expect(repo).toHaveAccessibleDescription(/credential of Alice, so only they can change/);
    expect(screen.getByRole("checkbox", { name: "Include pre-releases" })).toBeDisabled();
    expect(screen.getByLabelText("Name")).not.toHaveAttribute("readonly");
    expect(screen.getByLabelText("Schedule", { exact: true })).not.toHaveAttribute("readonly");
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await screen.findByRole("heading", { level: 1, name: "B" });
    const body = lastCall(fn, "/api/v1/watches/w1", "PATCH")?.body;
    expect(body).not.toHaveProperty("config");
    expect(body).toMatchObject({ name: "A", schedule: "15m" });
  });

  it("keeps the config editable for a rw sharee when no credential is attached", async () => {
    mockApi({
      "/api/v1/watches/w1": watch({ permission: "rw" }),
      "/api/v1/watches/w1/shares": { items: [] },
    });
    renderApp("/watches/w1/edit");
    expect(await screen.findByLabelText("Repository")).not.toHaveAttribute("readonly");
    expect(screen.queryByText(/target fields are read-only/)).not.toBeInTheDocument();
  });

  it("refuses a read-only share, a missing watch, and reports load errors", async () => {
    mockApi({ "/api/v1/watches/w1": watch({ permission: "ro" }) });
    const { unmount } = renderApp("/watches/w1/edit");
    expect(await screen.findByText("This watch is shared with you read-only.")).toBeInTheDocument();
    unmount();

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
