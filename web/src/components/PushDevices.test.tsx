import { createHash } from "node:crypto";

import { QueryClient } from "@tanstack/react-query";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { NO_PUSH, PUSH_DENIED, PUSH_DISMISSED, defaultDeviceName } from "@/lib/push";
import { CHANNEL_TYPES, mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { channel } from "@/test/routing";
import { lastCall } from "@/test/watches";
import type { PushDevice } from "@/types/api";

const NEW_ENDPOINT = "https://push.example/send/new";
const KEY = "BPublicVapidKey";
const hex = (s: string) => createHash("sha256").update(s).digest("hex");

function device(over: Partial<PushDevice> = {}): PushDevice {
  return {
    id: "d1",
    name: "Work laptop",
    user_agent: "Firefox/140",
    endpoint_sha256: hex("https://push.example/send/old"),
    created_at: "2026-11-01T00:00:00Z",
    last_push_at: null,
    ...over,
  };
}

const sub = (endpoint: string) => ({
  endpoint,
  toJSON: () => ({ endpoint, keys: { p256dh: "P256", auth: "AUTH" } }),
});

/** Stubs the Push API: permission, what the prompt answers, and an existing subscription. */
function stubPush({
  permission = "default" as NotificationPermission,
  answer = "granted" as NotificationPermission,
  endpoint = null as string | null,
} = {}) {
  let current = endpoint ? sub(endpoint) : null;
  const subscribe = vi.fn(async () => (current = sub(NEW_ENDPOINT)));
  const reg = { pushManager: { getSubscription: async () => current, subscribe } };
  const register = vi.fn().mockResolvedValue(reg);
  const N = {
    permission,
    requestPermission: vi.fn(async () => {
      N.permission = answer;
      return answer;
    }),
  };
  vi.stubGlobal("Notification", N);
  vi.stubGlobal("PushManager", vi.fn());
  Object.defineProperty(navigator, "serviceWorker", {
    value: Object.assign(new EventTarget(), {
      register,
      ready: Promise.resolve(reg),
      getRegistration: async () => reg,
    }),
    configurable: true,
  });
  return { subscribe, register, requestPermission: N.requestPermission };
}

/** A device API holding devices; a POST adds the registered one. */
function deviceApi(initial: PushDevice[] = [], extra: Record<string, unknown> = {}) {
  let items = initial;
  return mockApi({
    "/api/v1/push/key": { public_key: KEY },
    "/api/v1/push/devices": (_: URL, init?: RequestInit) => {
      if (init?.method !== "POST") return Response.json({ items });
      const body = JSON.parse(String(init.body)) as { name: string; endpoint: string };
      const d = device({ id: "d-new", name: body.name, endpoint_sha256: hex(body.endpoint) });
      items = [...items, d];
      return Response.json(d, { status: 201 });
    },
    "/api/v1/channels": (_: URL, init?: RequestInit) =>
      init?.method === "POST"
        ? Response.json(channel({ type: "web_push", config: { devices: "all" } }), { status: 201 })
        : Response.json({ items: [] }),
    ...extra,
  });
}

/** Waits for the device list and this browser's endpoint: the Web Push pick needs both (010-AC17). */
async function pushReady(client: QueryClient) {
  await waitFor(() => {
    expect(client.getQueryState(["push-devices"])?.status).not.toBe("pending");
    expect(client.getQueryState(["push-endpoint"])?.status).toBe("success");
  });
}

async function openNewChannel() {
  await userEvent.click(
    (await screen.findAllByRole("button", { name: "New channel" }))[0] as HTMLElement,
  );
  return screen.getByRole("dialog", { name: "New channel" });
}

/** Opens the edit form of the Web Push channel "Push", which picks d1. */
async function openPushChannel() {
  await userEvent.click(await screen.findByRole("button", { name: "Edit Push" }));
  return screen.getByRole("dialog", { name: "Edit Push" });
}

const pushChannel = (config: Record<string, unknown> = { devices: ["d1"], silent: false }) =>
  channel({ type: "web_push", name: "Push", config });

afterEach(() => {
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator, "serviceWorker");
});

describe("Web Push devices in the channel form", () => {
  it("names a device after its browser", () => {
    for (const [ua, want] of [
      [
        "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0",
        "Firefox on Linux",
      ],
      [
        "Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/140 Safari/537.36 Edg/140",
        "Edge on Windows",
      ],
      [
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_0) AppleWebKit/605 Version/18 Safari/605",
        "Safari on macOS",
      ],
      [
        "Mozilla/5.0 (Linux; Android 15) AppleWebKit/537.36 Chrome/140 Mobile Safari/537.36",
        "Chrome on Android",
      ],
      ["Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) Safari/604", "Safari on iOS"],
      ["curl/8", "Browser"],
    ]) {
      expect(defaultDeviceName(ua)).toBe(want);
    }
  });

  // 010-AC18: the Channels page lists channels only; devices live in the Web Push channel form.
  it("has no device section on the Channels page", async () => {
    deviceApi([device()]);
    renderApp("/channels");
    await screen.findByRole("heading", { level: 1, name: "Channels" });
    await waitFor(() => expect(screen.getAllByRole("region").length).toBeGreaterThan(0));
    expect(screen.queryByRole("region", { name: /device/i })).toBeNull();
    expect(screen.queryByRole("list", { name: "My devices" })).toBeNull();
    expect(screen.queryByText("Work laptop")).toBeNull();
    const dialog = await openNewChannel();
    await userEvent.click(within(dialog).getByText("Web Push"));
    expect(within(dialog).getByRole("list", { name: "My devices" })).toHaveTextContent(
      "Work laptop",
    );
  });

  // 010-AC14: no Push API: the form says why, and nothing registers.
  it("says why a browser without Push cannot be added", async () => {
    const fn = deviceApi();
    renderApp("/channels");
    const dialog = await openNewChannel();
    await userEvent.click(within(dialog).getByText("Web Push"));
    expect(within(dialog).getByText(NO_PUSH)).toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: "Add this browser" })).toBeNull();
    expect(lastCall(fn, "/api/v1/push/devices", "POST")).toBeUndefined();
  });

  // 010-AC14: a refused permission: no prompt again, the reason shows, nothing registers.
  it("says why when notifications are blocked", async () => {
    const push = stubPush({ permission: "denied" });
    const fn = deviceApi();
    const { client } = renderApp("/channels");
    await pushReady(client);
    const dialog = await openNewChannel();
    await userEvent.click(within(dialog).getByText("Web Push"));
    expect(within(dialog).getByText(PUSH_DENIED)).toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: "Add this browser" })).toBeNull();
    expect(push.requestPermission).not.toHaveBeenCalled();
    expect(lastCall(fn, "/api/v1/push/devices", "POST")).toBeUndefined();
  });

  // 010-AC17: picking Web Push prompts, from the change handler, before any other call; never on load or save.
  it("asks for permission when Web Push is picked, then registers this browser", async () => {
    const push = stubPush();
    const fn = deviceApi([device()]);
    // What had run when the prompt opened, and whether it opened inside the handler's own event dispatch.
    let atPrompt: { key: number; register: number; subscribe: number; event?: string } | null =
      null;
    push.requestPermission.mockImplementation(async () => {
      atPrompt = {
        key: fn.mock.calls.filter(([u]) => String(u).includes("/push/key")).length,
        register: push.register.mock.calls.length,
        subscribe: push.subscribe.mock.calls.length,
        event: window.event?.type,
      };
      return "granted";
    });
    const { client } = renderApp("/channels");
    await pushReady(client);
    const dialog = await openNewChannel();
    expect(push.requestPermission).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByText("Web Push"));
    expect(push.requestPermission).toHaveBeenCalledTimes(1);
    expect(atPrompt).toEqual({ key: 0, register: 0, subscribe: 0, event: expect.any(String) });
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/push/devices", "POST")?.body).toEqual({
        endpoint: NEW_ENDPOINT,
        keys: { p256dh: "P256", auth: "AUTH" },
        name: defaultDeviceName(),
        user_agent: navigator.userAgent,
      }),
    );
    expect(push.register).toHaveBeenCalledWith("/sw.js");
    expect(push.subscribe).toHaveBeenCalledWith({
      userVisibleOnly: true,
      applicationServerKey: KEY,
    });
    // Registered: the fallback button goes, the list marks it, and picking devices ticks it.
    await waitFor(() =>
      expect(within(dialog).queryByRole("button", { name: "Add this browser" })).toBeNull(),
    );
    expect(within(dialog).getByRole("list", { name: "My devices" })).toHaveTextContent(
      `${defaultDeviceName()}This browser`,
    );
    expect(within(dialog).getByLabelText(/All my devices/)).toBeChecked();
    expect(within(dialog).queryByRole("checkbox")).toBeNull();
    await userEvent.click(within(dialog).getByLabelText(/Pick devices/));
    const list = within(dialog).getByRole("list", { name: "My devices" });
    expect(within(list).getByRole("checkbox", { name: /This browser/ })).toBeChecked();
    expect(within(list).getByRole("checkbox", { name: "Work laptop" })).not.toBeChecked();
    await userEvent.click(within(list).getByRole("checkbox", { name: "Work laptop" }));
    await userEvent.click(within(dialog).getByRole("switch", { name: "Silent" }));
    await userEvent.type(within(dialog).getByLabelText("Name"), "Phone");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create channel" }));
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/channels", "POST")?.body).toEqual({
        name: "Phone",
        type: "web_push",
        config: { devices: ["d-new", "d1"], silent: true },
      }),
    );
    expect(push.requestPermission).toHaveBeenCalledTimes(1);
  });

  // 010-AC17: picking Web Push again while its prompt is open starts no second registration.
  it("does not prompt twice while a prompt is open", async () => {
    const push = stubPush();
    let answer: (p: NotificationPermission) => void = () => {};
    push.requestPermission.mockImplementation(
      () => new Promise<NotificationPermission>((r) => (answer = r)),
    );
    const fn = deviceApi();
    const { client } = renderApp("/channels");
    await pushReady(client);
    const dialog = await openNewChannel();
    await userEvent.click(within(dialog).getByText("Web Push"));
    await userEvent.click(within(dialog).getByText("Webhook"));
    await userEvent.click(within(dialog).getByText("Web Push"));
    expect(push.requestPermission).toHaveBeenCalledTimes(1);
    answer("granted");
    await waitFor(() => expect(lastCall(fn, "/api/v1/push/devices", "POST")).toBeDefined());
    expect(push.subscribe).toHaveBeenCalledTimes(1);
  });

  // 010-AC13: a clicked notification's link, posted by the service worker, routes the open tab in place.
  it("routes to a notification's link posted by the service worker", async () => {
    mockApi();
    const sw = new EventTarget();
    Object.defineProperty(navigator, "serviceWorker", { value: sw, configurable: true });
    const { router } = renderApp("/channels");
    await screen.findByRole("heading", { level: 1, name: "Channels" });
    const post = (data: unknown) =>
      act(() => {
        sw.dispatchEvent(new MessageEvent("message", { data }));
      });
    for (const data of [
      { url: "https://evil.test/inbox?event=1" },
      { url: "//evil.test/x" },
      { url: "http://[bad" },
      { url: 42 },
      null,
      "plain",
    ]) {
      post(data);
      expect(router.state.location.pathname).toBe("/channels");
    }
    post({ url: new URL("/watches?x=1#top", document.baseURI).href });
    expect(router.state.location).toMatchObject({
      pathname: "/watches",
      search: "?x=1",
      hash: "#top",
    });
  });

  // 010-AC13: a tab on a route outside the app shell (the signed-out page) follows the link too.
  it("routes from outside the app shell", async () => {
    mockApi();
    const sw = new EventTarget();
    Object.defineProperty(navigator, "serviceWorker", { value: sw, configurable: true });
    const { router } = renderApp("/auth/signed-out", null);
    await waitFor(() => expect(router.state.location.pathname).toBe("/auth/signed-out"));
    act(() => {
      sw.dispatchEvent(
        new MessageEvent("message", { data: { url: new URL("/watches", document.baseURI).href } }),
      );
    });
    expect(router.state.location.pathname).toBe("/watches");
  });

  // 010-AC17: a dismissed prompt leaves "Add this browser" as the fallback, which asks again.
  it("keeps Add this browser after a dismissed prompt", async () => {
    const push = stubPush({ answer: "default" });
    const fn = deviceApi();
    const { client } = renderApp("/channels");
    await pushReady(client);
    const dialog = await openNewChannel();
    await userEvent.click(within(dialog).getByText("Web Push"));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(PUSH_DISMISSED);
    expect(lastCall(fn, "/api/v1/push/devices", "POST")).toBeUndefined();
    // "All my devices" stays the default; with no device yet the list says so.
    expect(within(dialog).getByLabelText(/All my devices/)).toBeChecked();
    expect(within(dialog).getByText(/No device yet: add this browser/)).toBeInTheDocument();
    await userEvent.click(within(dialog).getByLabelText(/Pick devices/));
    push.requestPermission.mockResolvedValue("granted");
    await userEvent.click(within(dialog).getByRole("button", { name: "Add this browser" }));
    await waitFor(() => expect(lastCall(fn, "/api/v1/push/devices", "POST")).toBeDefined());
    expect(await within(dialog).findByRole("checkbox", { name: /This browser/ })).toBeChecked();
  });

  // 010-AC17: a browser that is already a device is not prompted; the list marks it.
  it("does not prompt a browser that is already a device", async () => {
    const push = stubPush({ permission: "granted", endpoint: "https://push.example/send/old" });
    deviceApi([device({ last_push_at: new Date().toISOString() })]);
    const { client } = renderApp("/channels");
    await pushReady(client);
    const dialog = await openNewChannel();
    await userEvent.click(within(dialog).getByText("Web Push"));
    const list = within(dialog).getByRole("list", { name: "My devices" });
    expect(within(list).getByText("This browser")).toBeInTheDocument();
    expect(list).toHaveTextContent("Firefox/140");
    expect(list).toHaveTextContent("Last push");
    expect(push.requestPermission).not.toHaveBeenCalled();
    expect(within(dialog).queryByRole("button", { name: "Add this browser" })).toBeNull();
  });

  // 010-AC14: a failed registration says why.
  it("tells when the registration fails", async () => {
    stubPush({ permission: "granted" });
    const fn = deviceApi([], {
      "/api/v1/push/key": () =>
        Response.json({ status: 503, detail: "Web Push is off" }, { status: 503 }),
    });
    const { client } = renderApp("/channels");
    await pushReady(client);
    const dialog = await openNewChannel();
    await userEvent.click(within(dialog).getByText("Web Push"));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("Web Push is off");
    expect(lastCall(fn, "/api/v1/push/devices", "POST")).toBeUndefined();
    expect(within(dialog).getByRole("button", { name: "Add this browser" })).toBeEnabled();
  });

  // 010-AC2, 010-AC18: rename, and remove after a confirm, from the channel form; the removed device leaves the picks.
  it("renames and removes a device in the channel form", async () => {
    let items = [device(), device({ id: "d2", name: "Phone" })];
    const fn = mockApi({
      "/api/v1/push/devices": () => Response.json({ items }),
      "/api/v1/push/devices/d1": (_: URL, init?: RequestInit) => {
        if (init?.method === "DELETE") {
          items = items.filter((d) => d.id !== "d1");
          return new Response(null, { status: 204 });
        }
        const { name } = JSON.parse(String(init?.body)) as { name: string };
        if (!name.trim())
          return Response.json({ status: 400, detail: "invalid device" }, { status: 400 });
        items = items.map((d) => (d.id === "d1" ? { ...d, name } : d));
        return Response.json(device({ name }));
      },
      "/api/v1/channels": { items: [pushChannel({ devices: ["d1", "d2"], silent: false })] },
      "/api/v1/channels/c1": (_: URL, init?: RequestInit) =>
        Response.json(pushChannel(JSON.parse(String(init?.body)).config)),
    });
    renderApp("/channels");
    const form = await openPushChannel();
    const list = within(form).getByRole("list", { name: "My devices" });
    expect(await within(list).findByText("Work laptop")).toBeInTheDocument();
    expect(list).toHaveTextContent("No push yet");
    await userEvent.click(within(list).getByRole("button", { name: "Rename Work laptop" }));
    let dialog = screen.getByRole("dialog", { name: "Rename Work laptop" });
    const input = within(dialog).getByLabelText("Name");
    await userEvent.clear(input);
    await userEvent.click(within(dialog).getByRole("button", { name: "Rename" }));
    expect(await within(dialog).findByText("invalid device")).toBeInTheDocument();
    await userEvent.type(input, "Desk");
    await userEvent.click(within(dialog).getByRole("button", { name: "Rename" }));
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: /^Rename/ })).not.toBeInTheDocument(),
    );
    expect(lastCall(fn, "/api/v1/push/devices/d1", "PATCH")?.body).toEqual({ name: "Desk" });
    // Closing a device dialog leaves the channel form open.
    expect(screen.getByRole("dialog", { name: "Edit Push" })).toBeInTheDocument();
    await userEvent.click(await within(list).findByRole("button", { name: "Rename Desk" }));
    dialog = screen.getByRole("dialog", { name: "Rename Desk" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("dialog", { name: "Edit Push" })).toBeInTheDocument();
    // Removal asks first, as channel deletion does; Cancel removes nothing.
    await userEvent.click(within(list).getByRole("button", { name: "Remove Desk" }));
    dialog = screen.getByRole("dialog", { name: "Remove Desk?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(lastCall(fn, "/api/v1/push/devices/d1", "DELETE")).toBeUndefined();
    expect(screen.getByRole("dialog", { name: "Edit Push" })).toBeInTheDocument();
    await userEvent.click(within(list).getByRole("button", { name: "Remove Desk" }));
    dialog = screen.getByRole("dialog", { name: "Remove Desk?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    expect(await screen.findByText("Removed Desk")).toBeInTheDocument();
    expect(screen.queryByRole("dialog", { name: /^Remove/ })).not.toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/push/devices/d1", "DELETE")).toBeDefined();
    await waitFor(() => expect(within(list).queryByText("Desk")).toBeNull());
    await userEvent.click(within(form).getByRole("button", { name: "Save channel" }));
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/channels/c1", "PATCH")?.body).toEqual({
        name: "Push",
        config: { devices: ["d2"], silent: false },
      }),
    );
  });

  // 010-AC18: in the new form too, with all devices targeted: rename and remove, no checkbox.
  it("manages devices from the new channel form", async () => {
    let items = [device()];
    const fn = mockApi({
      "/api/v1/push/devices": () => Response.json({ items }),
      "/api/v1/push/devices/d1": () => {
        items = [];
        return new Response(null, { status: 204 });
      },
    });
    renderApp("/channels");
    const form = await openNewChannel();
    await userEvent.click(within(form).getByText("Web Push"));
    const list = within(form).getByRole("list", { name: "My devices" });
    expect(within(list).queryByRole("checkbox")).toBeNull();
    expect(within(list).getByRole("button", { name: "Rename Work laptop" })).toBeInTheDocument();
    await userEvent.click(within(list).getByRole("button", { name: "Remove Work laptop" }));
    await userEvent.click(
      within(screen.getByRole("dialog", { name: "Remove Work laptop?" })).getByRole("button", {
        name: "Remove",
      }),
    );
    expect(await within(form).findByText(/No device yet/)).toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/push/devices/d1", "DELETE")).toBeDefined();
  });

  /** Devices whose DELETE drops them from the list. */
  function removableDevices(items: PushDevice[]) {
    return mockApi({
      "/api/v1/push/devices": () => Response.json({ items }),
      ...Object.fromEntries(
        items.map((d) => [
          `/api/v1/push/devices/${d.id}`,
          () => {
            items = items.filter((x) => x.id !== d.id);
            return new Response(null, { status: 204 });
          },
        ]),
      ),
      "/api/v1/channels": { items: [pushChannel()] },
    });
  }

  async function confirmRemove(form: HTMLElement, name: string) {
    await userEvent.click(within(form).getByRole("button", { name: `Remove ${name}` }));
    const dialog = screen.getByRole("dialog", { name: `Remove ${name}?` });
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: `Remove ${name}?` })).toBeNull(),
    );
  }

  // 010-AC15: a confirmed removal moves the focus to the next device, the previous one after the last row,
  // else to "Add this browser".
  it("keeps the focus in the form after a removal", async () => {
    stubPush({ permission: "granted" });
    removableDevices([
      device(),
      device({ id: "d2", name: "Phone" }),
      device({ id: "d3", name: "Tablet" }),
    ]);
    const { client } = renderApp("/channels");
    await pushReady(client);
    const form = await openPushChannel();
    await within(form).findByText("Tablet");
    await confirmRemove(form, "Phone");
    await waitFor(() =>
      expect(within(form).getByRole("button", { name: "Rename Tablet" })).toHaveFocus(),
    );
    await confirmRemove(form, "Tablet");
    await waitFor(() =>
      expect(within(form).getByRole("button", { name: "Rename Work laptop" })).toHaveFocus(),
    );
    await confirmRemove(form, "Work laptop");
    await waitFor(() =>
      expect(within(form).getByRole("button", { name: "Add this browser" })).toHaveFocus(),
    );
  });

  // 010-AC15: the removed row leaves the list at once, so the focus moves even when the refetch fails, and
  // only once: a later refetch leaves the focus where the user put it.
  it("moves the focus once after a removal whose refetch fails", async () => {
    let items = [device(), device({ id: "d2", name: "Phone" })];
    let listFails = false;
    mockApi({
      "/api/v1/push/devices": () =>
        listFails ? new Response("", { status: 500 }) : Response.json({ items }),
      "/api/v1/push/devices/d1": () => {
        items = items.filter((x) => x.id !== "d1");
        listFails = true;
        return new Response(null, { status: 204 });
      },
      "/api/v1/channels": { items: [pushChannel()] },
    });
    const { client } = renderApp("/channels");
    const form = await openPushChannel();
    await within(form).findByText("Phone");
    await confirmRemove(form, "Work laptop");
    await waitFor(() =>
      expect(within(form).getByRole("button", { name: "Rename Phone" })).toHaveFocus(),
    );
    expect(within(form).queryByText("Work laptop")).toBeNull();
    const name = within(form).getByLabelText("Name");
    name.focus();
    listFails = false;
    // New data, so the refocus effect runs again: it must have nothing left to do.
    items = [device({ id: "d2", name: "Phone 2" })];
    await act(() => client.invalidateQueries({ queryKey: ["push-devices"] }));
    expect(await within(form).findByText("Phone 2")).toBeInTheDocument();
    expect(name).toHaveFocus();
  });

  // 010-AC14, 010-AC15: a browser that cannot be added: the focus goes to the devices group, and the empty
  // list does not tell to add it.
  it("focuses the devices group after the last removal when this browser cannot be added", async () => {
    removableDevices([device()]);
    renderApp("/channels");
    const form = await openPushChannel();
    await within(form).findByText("Work laptop");
    await confirmRemove(form, "Work laptop");
    await waitFor(() => expect(within(form).getByRole("group", { name: "Devices" })).toHaveFocus());
    expect(within(form).getByText(NO_PUSH)).toBeInTheDocument();
    expect(within(form).getByText("No device yet.")).toBeInTheDocument();
    expect(within(form).queryByText(/add this browser/i)).toBeNull();
  });

  it("tells when a removal or the list fails", async () => {
    mockApi({
      "/api/v1/push/devices": { items: [device()] },
      "/api/v1/push/devices/d1": () => new Response("", { status: 500 }),
      "/api/v1/channels": { items: [pushChannel()] },
    });
    const first = renderApp("/channels");
    const form = await openPushChannel();
    await userEvent.click(await within(form).findByRole("button", { name: "Remove Work laptop" }));
    const dialog = screen.getByRole("dialog", { name: "Remove Work laptop?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      /Could not remove the device/,
    );
    first.unmount();
    mockApi({
      "/api/v1/push/devices": () => new Response("", { status: 500 }),
      "/api/v1/channels": { items: [pushChannel()] },
    });
    renderApp("/channels");
    const again = await openPushChannel();
    expect(await within(again).findByText("Devices could not be loaded.")).toBeInTheDocument();
    mockApi({
      "/api/v1/push/devices": { items: [device()] },
      "/api/v1/channels": { items: [pushChannel()] },
    });
    await userEvent.click(within(again).getByRole("button", { name: "Retry" }));
    expect(await within(again).findByRole("checkbox", { name: "Work laptop" })).toBeChecked();
  });

  // The server's reason when Web Push is off (no VAPID key), like other unavailable types.
  it("shows the server's hint when Web Push is off", async () => {
    stubPush();
    const hint = "Web Push is off: the stored VAPID key does not open with GEDOUR_ENCRYPTION_KEY";
    mockApi({
      "/api/v1/channel-types": {
        items: [...CHANNEL_TYPES, { type: "web_push", available: false, hint }],
      },
      "/api/v1/channels": { items: [pushChannel()] },
    });
    renderApp("/channels");
    const dialog = await openNewChannel();
    expect(within(dialog).getByRole("radio", { name: "Web Push" })).toBeDisabled();
    expect(dialog).toHaveTextContent(hint);
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    const edit = await openPushChannel();
    expect(await within(edit).findByText(hint)).toBeInTheDocument();
    expect(within(edit).queryByRole("button", { name: "Add this browser" })).toBeNull();
  });

  it("edits a Web Push channel's device list", async () => {
    const fn = mockApi({
      "/api/v1/push/devices": { items: [device(), device({ id: "d2", name: "Phone" })] },
      "/api/v1/channels": {
        items: [
          channel({ type: "web_push", name: "Push", config: { devices: ["d2"], silent: false } }),
          channel({ id: "c2", type: "web_push", name: "All", config: { devices: "all" } }),
        ],
      },
      "/api/v1/channels/c1": (_: URL, init?: RequestInit) =>
        Response.json(channel({ type: "web_push", config: JSON.parse(String(init?.body)).config })),
    });
    renderApp("/channels");
    const cards = within(await screen.findByRole("list", { name: "Channels" })).getAllByRole(
      "listitem",
    );
    expect(cards[0]).toHaveTextContent("Web Push · 1 device");
    expect(cards[1]).toHaveTextContent("Web Push · all devices");
    await userEvent.click(screen.getByRole("button", { name: "Edit Push" }));
    const dialog = screen.getByRole("dialog", { name: "Edit Push" });
    expect(within(dialog).getByLabelText(/Pick devices/)).toBeChecked();
    expect(within(dialog).getByRole("checkbox", { name: "Phone" })).toBeChecked();
    await userEvent.click(within(dialog).getByRole("checkbox", { name: "Phone" }));
    await userEvent.click(within(dialog).getByRole("checkbox", { name: "Work laptop" }));
    expect(within(dialog).queryByLabelText(/secret|token/i)).toBeNull();
    await userEvent.click(within(dialog).getByRole("button", { name: "Save channel" }));
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/channels/c1", "PATCH")?.body).toEqual({
        name: "Push",
        config: { devices: ["d1"], silent: false },
      }),
    );
  });

  it("shows a device list error at the device picker", async () => {
    mockApi({
      "/api/v1/push/devices": { items: [device()] },
      "/api/v1/channels": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? Response.json(
              {
                status: 400,
                detail: "invalid channel",
                errors: [
                  { location: "body.config.devices[0]", message: "no such device among yours" },
                ],
              },
              { status: 400 },
            )
          : Response.json({ items: [] }),
    });
    renderApp("/channels");
    const dialog = await openNewChannel();
    await userEvent.click(within(dialog).getByText("Web Push"));
    await userEvent.click(within(dialog).getByLabelText(/Pick devices/));
    await userEvent.click(within(dialog).getByRole("checkbox", { name: "Work laptop" }));
    await userEvent.type(within(dialog).getByLabelText("Name"), "x");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create channel" }));
    expect(await within(dialog).findByText("no such device among yours")).toBeInTheDocument();
    expect(within(dialog).queryByText(/devices\[0\]:/)).toBeNull();
    await userEvent.click(within(dialog).getByLabelText(/All my devices/));
    expect(within(dialog).queryByRole("checkbox", { name: "Work laptop" })).toBeNull();
  });

  // A Web Push test some devices refused is sent, with a warning naming them.
  it("warns when some devices refused the test", async () => {
    const fn = mockApi({
      "/api/v1/channels": { items: [channel({ type: "web_push", config: { devices: "all" } })] },
      "/api/v1/channels/c1/test": {
        ok: true,
        accepted: 1,
        error: "POST push.example: HTTP 403",
      },
    });
    renderApp("/channels");
    await userEvent.click(await screen.findByRole("button", { name: "Send test" }));
    expect(
      await screen.findByText(
        "Test message sent to Deploy bot, but some devices refused it: POST push.example: HTTP 403",
      ),
    ).toBeInTheDocument();
    // The list refetches: the devices that took it show their new last push.
    await waitFor(() =>
      expect(fn.mock.calls.filter(([u]) => String(u).endsWith("/push/devices")).length).toBe(2),
    );
  });
});
