import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  BellPlus,
  Check,
  Copy,
  Pencil,
  Plus,
  RotateCw,
  Send,
  Trash2,
  TriangleAlert,
} from "lucide-react";
import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import { Link } from "react-router";

import {
  ApiError,
  createChannel,
  deleteChannel,
  listChannels,
  listChannelTypes,
  listDeliveries,
  listScripts,
  retryDelivery,
  testChannel,
  updateChannel,
} from "@/api/client";
import { ChannelIcon, DeliveryBadge } from "@/components/ChannelBits";
import { PushDevicesSection } from "@/components/PushDevices";
import { EmptyState } from "@/components/EmptyState";
import { ErrorState } from "@/components/ErrorState";
import { Timestamp } from "@/components/Timestamp";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { Field, inputClass } from "@/components/ui/field";
import { useToast } from "@/hooks/toast";
import { DEVICES_KEY, useAddBrowser, usePushDevices, usePushUnavailable } from "@/hooks/usePush";
import {
  browserTimeZone,
  CHANNEL_KINDS,
  channelKind,
  channelTarget,
  DELIVERY_STATUS,
  DIGEST_TYPES,
  digestErrors,
  digestSummary,
  MAX_DIGEST_TIMES,
  timeZones,
} from "@/lib/channels";
import { summarizeEvent } from "@/lib/events";
import { fieldErrors, problemText, unplaced } from "@/lib/problems";
import { defaultDeviceName, pushBlocked } from "@/lib/push";
import { formatRelative } from "@/lib/time";
import { cn } from "@/lib/utils";
import type { NavItem } from "@/nav";
import type {
  Channel,
  ChannelKind,
  CreatedChannel,
  Delivery,
  DeliveryFilters,
  DeliveryStatus,
} from "@/types/api";

const sectionClass = "flex flex-col gap-4";

/** Script names by id, for the channels that run one. */
function useScriptNames(): Map<string, string> {
  const scripts = useQuery({ queryKey: ["scripts"], queryFn: listScripts });
  return new Map((scripts.data ?? []).map((s) => [s.id, s.name]));
}

export function ChannelsPage({ item }: { item: NavItem }) {
  const query = useQuery({ queryKey: ["channels"], queryFn: listChannels });
  const scriptNames = useScriptNames();
  const [editing, setEditing] = useState<Channel | "new" | null>(null);
  const [deleting, setDeleting] = useState<Channel | null>(null);
  const newButton = (
    <Button variant="primary" onClick={() => setEditing("new")}>
      <Plus aria-hidden />
      New channel
    </Button>
  );
  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-10 px-5 py-8 md:px-10 md:py-12">
      <title>{`${item.label} – gedour`}</title>
      <section className={sectionClass} aria-labelledby="channels-title">
        <div className="flex flex-wrap items-end justify-between gap-4">
          <div className="flex flex-col gap-2">
            <h1 id="channels-title" className="font-display text-3xl font-semibold tracking-tight">
              {item.label}
            </h1>
            <p className="max-w-prose text-muted">
              Where your rules send matching changes, besides the inbox. Secrets (bot tokens,
              signing keys) are stored encrypted and never shown again.
            </p>
          </div>
          {newButton}
        </div>
        {query.isError && !query.data ? (
          <ErrorState
            title="Channels could not be loaded."
            error={query.error}
            onRetry={() => void query.refetch()}
          />
        ) : query.isPending ? (
          <ul aria-busy aria-label="Loading channels" className="grid gap-3 md:grid-cols-2">
            {Array.from({ length: 2 }, (_, i) => (
              <li
                key={i}
                data-testid="channel-skeleton"
                className="flex h-28 flex-col gap-2 rounded-xl border border-line bg-surface p-4"
              >
                <span aria-hidden className="h-3.5 w-32 animate-pulse rounded bg-line" />
                <span aria-hidden className="h-3 w-48 animate-pulse rounded bg-line" />
              </li>
            ))}
          </ul>
        ) : query.data.length === 0 ? (
          <EmptyState title={item.emptyTitle} body={item.emptyBody} action={newButton} />
        ) : (
          <ul aria-label="Channels" className="grid grid-cols-1 gap-3 md:grid-cols-2">
            {query.data.map((c) => (
              <ChannelCard
                key={c.id}
                channel={c}
                target={channelTarget(c, scriptNames)}
                onEdit={() => setEditing(c)}
                onDelete={() => setDeleting(c)}
              />
            ))}
          </ul>
        )}
      </section>
      <PushDevicesSection />
      <DeliveriesSection channels={query.data ?? []} />
      <ChannelDialog editing={editing} onClose={() => setEditing(null)} />
      <DeleteDialog channel={deleting} onClose={() => setDeleting(null)} />
    </div>
  );
}

function ChannelCard({
  channel: c,
  target,
  onEdit,
  onDelete,
}: {
  channel: Channel;
  target: string;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const toast = useToast();
  const client = useQueryClient();
  const script = c.type === "script";
  const digest = digestSummary(c.config);
  const test = useMutation({
    mutationFn: () => testChannel(c.id),
    onSuccess: (r) => {
      // 010: ok with an error is a Web Push test some devices refused.
      if (r.ok && r.error)
        toast(`Test message sent to ${c.name}, but some devices refused it: ${r.error}`, "warning");
      else if (r.ok)
        toast(script ? `Test run of ${c.name} done` : `Test message sent to ${c.name}`);
      else toast(`Test to ${c.name} failed: ${r.error ?? "unknown error"}`, "error");
      void client.invalidateQueries({ queryKey: ["audit"] });
      // A push stamps its devices' last push.
      if (c.type === "web_push") void client.invalidateQueries({ queryKey: DEVICES_KEY });
    },
    onError: (err) => toast(problemText(err), "error"),
  });
  // A script test is a live run: what it logged shows under the card.
  const run = script && test.data ? test.data : null;
  return (
    <li className="flex flex-col gap-3 rounded-xl border border-line bg-surface p-4">
      <div className="flex items-start gap-3">
        <span className="grid size-10 shrink-0 place-items-center rounded-lg bg-beacon-soft text-ink ring-1 ring-beacon/25 ring-inset">
          <ChannelIcon type={c.type} className="size-5" />
        </span>
        <div className="flex min-w-0 flex-1 flex-col gap-0.5">
          <span className="truncate font-medium">{c.name}</span>
          <span className="truncate text-xs text-muted">
            {channelKind(c.type).label} · <span className="font-mono">{target}</span>
          </span>
          {digest && <span className="truncate text-xs text-muted">{digest}</span>}
        </div>
      </div>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="flex items-center gap-2 text-xs text-muted">
          {c.last_delivery_status ? (
            <>
              <DeliveryBadge status={c.last_delivery_status} />
              {c.last_delivery_at && formatRelative(new Date(c.last_delivery_at))}
            </>
          ) : (
            "No delivery yet"
          )}
        </span>
        <span className="flex gap-1">
          <Button
            variant="outline"
            size="sm"
            disabled={test.isPending}
            onClick={() => test.mutate()}
          >
            <Send aria-hidden />
            {test.isPending
              ? script
                ? "Running…"
                : "Sending…"
              : script
                ? "Run test"
                : "Send test"}
          </Button>
          <Button variant="ghost" size="sm" aria-label={`Edit ${c.name}`} onClick={onEdit}>
            <Pencil aria-hidden />
          </Button>
          <Button variant="ghost" size="sm" aria-label={`Delete ${c.name}`} onClick={onDelete}>
            <Trash2 aria-hidden />
          </Button>
        </span>
      </div>
      {run && (
        <section
          aria-label={`Test run of ${c.name}`}
          className="flex flex-col gap-2 rounded-lg bg-bg px-3 py-2 text-xs"
        >
          <p className="flex items-center justify-between gap-2">
            <span
              className={
                run.ok
                  ? "text-emerald-700 dark:text-emerald-300"
                  : "text-rose-700 dark:text-rose-300"
              }
            >
              {run.ok ? "Live run returned" : `Live run failed: ${run.error ?? ""}`}
            </span>
            <button
              type="button"
              className="text-muted underline-offset-4 hover:underline"
              onClick={() => test.reset()}
            >
              Hide
            </button>
          </p>
          <p className="text-muted">Not a dry run: its http.fetch calls were sent.</p>
          <pre className="max-h-40 overflow-auto font-mono break-words whitespace-pre-wrap">
            {run.logs?.length ? run.logs.join("\n") : "No log line."}
          </pre>
        </section>
      )}
    </li>
  );
}

function splitRecipients(s: string): string[] {
  return s
    .split(/[\n,;]+/)
    .map((x) => x.trim())
    .filter(Boolean);
}

function ChannelDialog({
  editing,
  onClose,
}: {
  editing: Channel | "new" | null;
  onClose: () => void;
}) {
  const ch = editing && editing !== "new" ? editing : null;
  // While a generated secret shows, only its own button closes the dialog: it is the one chance to copy it.
  const [locked, setLocked] = useState(false);
  const close = () => {
    setLocked(false);
    onClose();
  };
  return (
    <Dialog
      open={editing !== null}
      title={ch ? `Edit ${ch.name}` : "New channel"}
      onClose={close}
      dismissible={!locked}
      className="w-[min(32rem,calc(100vw-2rem))]"
    >
      {editing !== null && (
        <ChannelForm
          key={ch?.id ?? "new"}
          channel={ch}
          onClose={close}
          onSecret={() => setLocked(true)}
        />
      )}
    </Dialog>
  );
}

function ChannelForm({
  channel,
  onClose,
  onSecret,
}: {
  channel: Channel | null;
  onClose: () => void;
  onSecret: () => void;
}) {
  const types = useQuery({
    queryKey: ["channel-types"],
    queryFn: listChannelTypes,
    staleTime: Infinity,
  });
  const [type, setType] = useState<ChannelKind>(channel?.type ?? "webhook");
  const [name, setName] = useState(channel?.name ?? "");
  const cfg = channel?.config ?? {};
  const [chatId, setChatId] = useState(String(cfg.chat_id ?? ""));
  const [to, setTo] = useState(Array.isArray(cfg.to) ? cfg.to.join("\n") : "");
  const [url, setUrl] = useState(String(cfg.url ?? ""));
  const [conversation, setConversation] = useState(String(cfg.conversation ?? ""));
  const [silent, setSilent] = useState(cfg.silent === true);
  const [scriptId, setScriptId] = useState(String(cfg.script_id ?? ""));
  const [picked, setPicked] = useState<string[] | null>(
    Array.isArray(cfg.devices) ? cfg.devices.map(String) : null,
  );
  const push = usePushDevices();
  const tick = (id: string) => setPicked((p) => (p && !p.includes(id) ? [...p, id] : p));
  // Known before the endpoint check catches up: a pick right after the registration still ticks it.
  const [added, setAdded] = useState<string | null>(null);
  const browser = useAddBrowser((d) => {
    setAdded(d.id);
    tick(d.id);
  });
  const here = push.thisId ?? added;
  const pushOff = usePushUnavailable();
  // 010-AC17: picking Web Push is the gesture that asks for permission, when this browser is not a device yet.
  const pick = (k: ChannelKind) => {
    setType(k);
    if (
      k === "web_push" &&
      push.thisId === null &&
      added === null &&
      !browser.busy &&
      pushBlocked() === null
    )
      browser.add(defaultDeviceName());
  };
  const scripts = useQuery({ queryKey: ["scripts"], queryFn: listScripts });
  const [secret, setSecret] = useState("");
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [signing, setSigning] = useState<{ name: string; secret: string } | null>(null);
  const client = useQueryClient();
  const toast = useToast();
  const available = types.data?.find((t) => t.type === type);

  const [digest, setDigest] = useState(cfg.delivery === "digest");
  const [times, setTimes] = useState<string[]>(
    Array.isArray(cfg.at) ? cfg.at.map(String) : ["09:00"],
  );
  const [tz, setTz] = useState(typeof cfg.tz === "string" ? cfg.tz : browserTimeZone);
  const digestOn = digest && DIGEST_TYPES.includes(type);

  const typeConfig = () => {
    if (type === "telegram") return { chat_id: chatId, silent };
    if (type === "nextcloud_talk") return { url, conversation, silent };
    if (type === "email") return { to: splitRecipients(to) };
    if (type === "script") return { script_id: scriptId };
    if (type === "web_push") return { devices: picked ?? "all", silent };
    return { url };
  };
  // Immediate is the server default: its keys are left out, so the payload stays as before 012.
  const config = () =>
    digestOn
      ? { ...typeConfig(), delivery: "digest", at: times, tz: tz.trim() || "UTC" }
      : typeConfig();
  const save = useMutation({
    // The response may hold the signing secret: nothing keeps it once this form is gone.
    gcTime: 0,
    mutationFn: async () => {
      if (channel) {
        return updateChannel(channel.id, { name, config: config(), secret: secret || undefined });
      }
      return createChannel({ name, type, config: config(), secret: secret || undefined });
    },
    onSuccess: (c) => {
      void client.invalidateQueries({ queryKey: ["channels"] });
      setSecret("");
      // A generated signing secret is in this response only: show it before closing.
      const generated = (c as CreatedChannel).signing_secret;
      if (generated) {
        setSigning({ name: c.name, secret: generated });
        onSecret();
        return;
      }
      toast(`Saved ${c.name}`);
      onClose();
    },
    onError: (err) => setErrors(fieldErrors(err)),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const local = digestOn ? digestErrors(times, tz.trim()) : {};
    setErrors(local);
    if (Object.keys(local).length === 0) save.mutate();
  };

  if (signing)
    return <SigningSecret name={signing.name} secret={signing.secret} onDone={onClose} />;

  const deviceError =
    errors.devices ?? Object.entries(errors).find(([k]) => k.startsWith("devices["))?.[1];
  const secretHint = channel
    ? "Write-only. Leave empty to keep the current one."
    : type === "webhook"
      ? "Write-only. Leave empty to have a strong one generated and shown once."
      : "Write-only: gedour never shows it again.";
  const shown = [
    "name",
    "type",
    "secret",
    "chat_id",
    "url",
    "conversation",
    "to",
    "script_id",
    "devices",
    "delivery",
    "at",
    "tz",
  ];
  return (
    <form noValidate onSubmit={submit} aria-label="Channel" className="flex flex-col gap-4">
      {unplaced(errors, [
        ...shown,
        ...Object.keys(errors).filter(
          (k) => k.startsWith("to[") || k.startsWith("devices[") || k.startsWith("at["),
        ),
      ]).map((m) => (
        <p key={m} role="alert" className="text-sm text-rose-700 dark:text-rose-300">
          {m}
        </p>
      ))}
      {!channel && (
        <fieldset className="flex flex-col gap-1.5">
          <legend className="mb-1.5 text-sm font-medium">Type</legend>
          <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
            {CHANNEL_KINDS.map((k) => {
              const t = types.data?.find((x) => x.type === k.type);
              const off = t ? !t.available : false;
              return (
                <label
                  key={k.type}
                  title={t?.hint}
                  className={cn(
                    "flex cursor-pointer flex-col items-center gap-1 rounded-lg border border-line px-2 py-3 text-center text-sm has-checked:border-beacon has-checked:bg-beacon-soft/50",
                    off && "cursor-not-allowed opacity-50",
                  )}
                >
                  <input
                    type="radio"
                    name="channel-type"
                    value={k.type}
                    checked={type === k.type}
                    disabled={off}
                    onChange={() => pick(k.type)}
                    className="sr-only"
                  />
                  <k.icon aria-hidden className="size-5" />
                  {k.label}
                </label>
              );
            })}
          </div>
          {types.data?.some((t) => !t.available) && (
            <p className="text-xs text-muted">
              {types.data
                .filter((t) => !t.available)
                .map((t) => t.hint)
                .join(" ")}
            </p>
          )}
          {errors.type && <p className="text-sm text-rose-700 dark:text-rose-300">{errors.type}</p>}
        </fieldset>
      )}
      <Field label="Name" error={errors.name}>
        {(p) => (
          <input
            {...p}
            value={name}
            maxLength={200}
            onChange={(e) => setName(e.target.value)}
            className={inputClass}
          />
        )}
      </Field>
      {type === "telegram" && (
        <Field
          label="Chat ID"
          error={errors.chat_id}
          hint="Numeric id (e.g. -1001234567890) or @channelname; the bot must be in the chat."
        >
          {(p) => (
            <input
              {...p}
              value={chatId}
              onChange={(e) => setChatId(e.target.value)}
              className={cn(inputClass, "font-mono")}
            />
          )}
        </Field>
      )}
      {type === "nextcloud_talk" && (
        <>
          <p className="text-xs text-muted">
            On the Nextcloud server, create the bot:{" "}
            <code className="font-mono break-all">
              occ talk:bot:create -s &lt;secret&gt; gedour
            </code>{" "}
            (Talk 22.0.1 and later) or{" "}
            <code className="font-mono break-all">
              occ talk:bot:install --feature response &lt;name&gt; &lt;secret&gt; &lt;url&gt;
            </code>{" "}
            (older servers). Then enable it in the conversation:{" "}
            <code className="font-mono break-all">
              occ talk:bot:setup &lt;bot-id&gt; &lt;token&gt;
            </code>
            .
          </p>
          <Field
            label="Nextcloud URL"
            error={errors.url}
            hint="The base URL of the Nextcloud server. Redirects are not followed."
          >
            {(p) => (
              <input
                {...p}
                type="url"
                placeholder="https://cloud.example.com"
                value={url}
                onChange={(e) => setUrl(e.target.value)}
                className={cn(inputClass, "font-mono")}
              />
            )}
          </Field>
          <Field
            label="Conversation token"
            error={errors.conversation}
            hint="The end of the conversation's link: /call/<token>."
          >
            {(p) => (
              <input
                {...p}
                value={conversation}
                spellCheck={false}
                onChange={(e) => setConversation(e.target.value)}
                className={cn(inputClass, "font-mono")}
              />
            )}
          </Field>
        </>
      )}
      {type === "email" && (
        <Field
          label="Recipients"
          error={errors.to ?? Object.entries(errors).find(([k]) => k.startsWith("to["))?.[1]}
          hint="Up to 10 addresses, one per line or comma-separated."
        >
          {(p) => (
            <textarea
              {...p}
              rows={3}
              value={to}
              onChange={(e) => setTo(e.target.value)}
              className={cn(inputClass, "h-auto py-2")}
            />
          )}
        </Field>
      )}
      {type === "webhook" && (
        <Field
          label="URL"
          error={errors.url}
          hint="gedour POSTs JSON signed with X-Gedour-Signature (HMAC-SHA256). Redirects are not followed."
        >
          {(p) => (
            <input
              {...p}
              type="url"
              placeholder="https://hooks.example.com/gedour"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              className={cn(inputClass, "font-mono")}
            />
          )}
        </Field>
      )}
      {type === "script" && (
        <Field
          label="Script"
          error={errors.script_id}
          hint="Runs in the sandbox for each event: 5 s, log and http.fetch only."
        >
          {(p) =>
            scripts.data?.length === 0 ? (
              <p className="text-sm">
                No scripts yet.{" "}
                <Link to="/scripts/new" className="font-medium underline underline-offset-4">
                  Write one
                </Link>{" "}
                first.
              </p>
            ) : (
              <select
                {...p}
                value={scriptId}
                onChange={(e) => setScriptId(e.target.value)}
                className={inputClass}
              >
                <option value="">Pick a script…</option>
                {(scripts.data ?? []).map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name}
                  </option>
                ))}
              </select>
            )
          }
        </Field>
      )}
      {type === "web_push" && (
        <fieldset className="flex flex-col gap-2">
          <legend className="mb-1.5 text-sm font-medium">Devices</legend>
          {[
            { all: true, label: "All my devices", hint: "Devices added later included." },
            { all: false, label: "Pick devices", hint: "Only the ones ticked below." },
          ].map((o) => (
            <label key={o.label} className="flex items-start gap-2 text-sm">
              <input
                type="radio"
                name="push-devices"
                checked={(picked === null) === o.all}
                onChange={() => setPicked(o.all ? null : here ? [here] : [])}
                className="mt-0.5 accent-beacon"
              />
              <span>
                {o.label} <span className="text-xs text-muted">{o.hint}</span>
              </span>
            </label>
          ))}
          {picked !== null &&
            (push.devices.data?.length ? (
              <ul aria-label="Devices to notify" className="flex flex-col gap-1.5 pl-6">
                {push.devices.data.map((d) => (
                  <li key={d.id}>
                    <label className="flex items-center gap-2 text-sm">
                      <input
                        type="checkbox"
                        checked={picked.includes(d.id)}
                        onChange={(e) =>
                          setPicked(
                            e.target.checked ? [...picked, d.id] : picked.filter((x) => x !== d.id),
                          )
                        }
                        className="accent-beacon"
                      />
                      <span className="min-w-0 wrap-anywhere">{d.name}</span>
                      {d.id === here && <span className="text-xs text-muted">(this browser)</span>}
                    </label>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="pl-6 text-sm text-muted">
                No device yet: add this browser, or add others from the Channels page in their own
                browser.
              </p>
            ))}
          {deviceError && <p className="text-sm text-rose-700 dark:text-rose-300">{deviceError}</p>}
          {pushOff ? (
            <p className="text-sm text-muted">{pushOff}</p>
          ) : (
            push.thisId === null &&
            here === null && (
              <Button
                type="button"
                variant="outline"
                className="self-start"
                disabled={browser.busy}
                onClick={() => browser.add(defaultDeviceName())}
              >
                <BellPlus aria-hidden />
                {browser.busy ? "Adding…" : "Add this browser"}
              </Button>
            )
          )}
          {browser.error && (
            <p role="alert" className="text-sm text-rose-700 dark:text-rose-300">
              {browser.error}
            </p>
          )}
        </fieldset>
      )}
      {type !== "email" && type !== "script" && type !== "web_push" && (
        <Field
          label={
            type === "telegram"
              ? "Bot token"
              : type === "nextcloud_talk"
                ? "Bot secret"
                : "Signing secret"
          }
          error={errors.secret}
          hint={secretHint}
        >
          {(p) => (
            <input
              {...p}
              type="password"
              autoComplete="new-password"
              value={secret}
              placeholder={channel?.last4 ? `••••${channel.last4}` : undefined}
              onChange={(e) => setSecret(e.target.value)}
              className={cn(inputClass, "font-mono")}
            />
          )}
        </Field>
      )}
      {(type === "telegram" || type === "nextcloud_talk" || type === "web_push") && (
        <Field
          label="Silent"
          hint={
            type === "nextcloud_talk" ? "Posts without a notification." : "Notifies without sound."
          }
        >
          {(p) => (
            <button
              {...p}
              type="button"
              role="switch"
              aria-checked={silent}
              onClick={() => setSilent(!silent)}
              className={cn(
                "relative h-6 w-11 shrink-0 self-start rounded-full transition-colors",
                silent ? "bg-beacon" : "bg-muted",
              )}
            >
              <span
                aria-hidden
                className={cn(
                  "absolute top-0.5 left-0.5 size-5 rounded-full bg-surface shadow transition-transform motion-reduce:transition-none",
                  silent && "translate-x-5",
                )}
              />
            </button>
          )}
        </Field>
      )}
      {DIGEST_TYPES.includes(type) && (
        <DeliveryFields
          digest={digest}
          setDigest={setDigest}
          times={times}
          setTimes={(v) => {
            setTimes(v);
            // A schedule error is about the rows that were there: it goes when they change.
            setErrors((e) =>
              Object.fromEntries(
                Object.entries(e).filter(([k]) => k !== "at" && !k.startsWith("at[")),
              ),
            );
          }}
          tz={tz}
          setTz={setTz}
          errors={errors}
        />
      )}
      <DialogActions>
        <Button type="button" variant="ghost" onClick={onClose}>
          Cancel
        </Button>
        <Button
          type="submit"
          variant="primary"
          disabled={save.isPending || available?.available === false}
        >
          {save.isPending ? "Saving…" : channel ? "Save channel" : "Create channel"}
        </Button>
      </DialogActions>
    </form>
  );
}

/** 012: immediate or digest, and the digest's daily times and time zone. */
function DeliveryFields({
  digest,
  setDigest,
  times,
  setTimes,
  tz,
  setTz,
  errors,
}: {
  digest: boolean;
  setDigest: (v: boolean) => void;
  times: string[];
  setTimes: (v: string[]) => void;
  tz: string;
  setTz: (v: string) => void;
  errors: Record<string, string>;
}) {
  const [zones] = useState(timeZones);
  const id = useId();
  const zoneList = `${id}-zones`;
  const deliveryErrorId = `${id}-delivery-error`;
  const list = useRef<HTMLUListElement>(null);
  const addButton = useRef<HTMLButtonElement>(null);
  // Row to focus once times re-render: -1 is the Add button.
  const focusRow = useRef<number | null>(null);
  useEffect(() => {
    const i = focusRow.current;
    if (i === null) return;
    focusRow.current = null;
    (i < 0 ? addButton.current : list.current?.querySelectorAll("input")[i])?.focus();
  }, [times]);
  return (
    <fieldset className="flex flex-col gap-2">
      <legend className="mb-1.5 text-sm font-medium">Delivery</legend>
      {[
        { on: false, label: "Immediate", hint: "One message per event, as it comes." },
        {
          on: true,
          label: "Digest",
          hint: "One message at set times, listing the events still unread. High priority goes out at once.",
        },
      ].map((o) => (
        <label key={o.label} className="flex items-start gap-2 text-sm">
          <input
            type="radio"
            name="delivery"
            checked={digest === o.on}
            aria-describedby={errors.delivery ? deliveryErrorId : undefined}
            onChange={() => setDigest(o.on)}
            className="mt-0.5 accent-beacon"
          />
          <span>
            {o.label} <span className="text-xs text-muted">{o.hint}</span>
          </span>
        </label>
      ))}
      {errors.delivery && (
        <p id={deliveryErrorId} className="text-sm text-rose-700 dark:text-rose-300">
          {errors.delivery}
        </p>
      )}
      {digest && (
        <div className="flex flex-col gap-3 pl-6">
          <ul ref={list} aria-label="Digest times" className="flex flex-col gap-1.5">
            {times.map((v, i) => {
              const error = errors[`at[${i}]`];
              const errorId = `${id}-at-${i}`;
              return (
                <li key={i} className="flex flex-wrap items-center gap-2">
                  <input
                    type="time"
                    aria-label={`Time ${i + 1}`}
                    aria-invalid={error ? true : undefined}
                    aria-describedby={error ? errorId : undefined}
                    value={v}
                    onChange={(e) => setTimes(times.map((x, j) => (j === i ? e.target.value : x)))}
                    className={cn(inputClass, "w-32 font-mono")}
                  />
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    aria-label={`Remove time ${i + 1}`}
                    disabled={times.length === 1}
                    onClick={() => {
                      focusRow.current = i - 1;
                      setTimes(times.filter((_, j) => j !== i));
                    }}
                  >
                    <Trash2 aria-hidden />
                  </Button>
                  {error && (
                    <p id={errorId} className="w-full text-sm text-rose-700 dark:text-rose-300">
                      {error}
                    </p>
                  )}
                </li>
              );
            })}
          </ul>
          {errors.at && <p className="text-sm text-rose-700 dark:text-rose-300">{errors.at}</p>}
          <Button
            ref={addButton}
            type="button"
            variant="outline"
            size="sm"
            className="self-start"
            disabled={times.length >= MAX_DIGEST_TIMES}
            onClick={() => {
              focusRow.current = times.length;
              setTimes([...times, ""]);
            }}
          >
            <Plus aria-hidden />
            Add a time
          </Button>
          <Field label="Time zone" error={errors.tz} hint="The times are local to this zone.">
            {(p) => (
              <>
                <input
                  {...p}
                  list={zoneList}
                  value={tz}
                  spellCheck={false}
                  autoComplete="off"
                  placeholder="UTC"
                  onChange={(e) => setTz(e.target.value)}
                  className={cn(inputClass, "font-mono")}
                />
                <datalist id={zoneList}>
                  {zones.map((z) => (
                    <option key={z} value={z} />
                  ))}
                </datalist>
              </>
            )}
          </Field>
        </div>
      )}
    </fieldset>
  );
}

/** The one time a generated webhook signing secret is visible. */
export function SigningSecret({
  name,
  secret,
  onDone,
}: {
  name: string;
  secret: string;
  onDone: () => void;
}) {
  const [copied, setCopied] = useState(false);
  const id = useId();
  return (
    <div className="flex flex-col gap-4" data-testid="signing-secret">
      <p className="text-sm">
        <strong>{name}</strong> is ready. Configure the receiver with this signing secret to check{" "}
        <code className="font-mono text-xs">X-Gedour-Signature</code>.
      </p>
      <div className="flex flex-col gap-1.5">
        <label htmlFor={id} className="text-sm font-medium">
          Signing secret
        </label>
        <div className="flex gap-2">
          <input
            id={id}
            readOnly
            value={secret}
            onFocus={(e) => e.target.select()}
            className={cn(inputClass, "font-mono text-xs read-only:text-ink")}
          />
          <Button
            type="button"
            variant="outline"
            onClick={() => {
              void navigator.clipboard?.writeText(secret).then(() => setCopied(true));
            }}
          >
            {copied ? <Check aria-hidden /> : <Copy aria-hidden />}
            {copied ? "Copied" : "Copy"}
          </Button>
        </div>
      </div>
      <p className="inline-flex items-start gap-2 rounded-lg bg-amber-500/10 px-3 py-2 text-sm text-amber-900 dark:text-amber-200">
        <TriangleAlert aria-hidden className="mt-0.5 size-4 shrink-0" />
        It is shown only now. If you lose it, set a new one by editing the channel.
      </p>
      <DialogActions>
        <Button variant="primary" onClick={onDone}>
          I have copied it
        </Button>
      </DialogActions>
    </div>
  );
}

function DeleteDialog({ channel, onClose }: { channel: Channel | null; onClose: () => void }) {
  const [inUse, setInUse] = useState<string[] | null>(null);
  const [error, setError] = useState("");
  const client = useQueryClient();
  const toast = useToast();
  const close = () => {
    setInUse(null);
    setError("");
    onClose();
  };
  const del = useMutation({
    mutationFn: () => deleteChannel(channel?.id ?? ""),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["channels"] });
      void client.invalidateQueries({ queryKey: ["deliveries"] });
      toast(`Deleted ${channel?.name ?? "the channel"}`);
      close();
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 409)
        setInUse(err.errors.map((e) => String(e.value)));
      else setError(problemText(err));
    },
  });
  return (
    <Dialog open={channel !== null} title={`Delete ${channel?.name ?? ""}?`} onClose={close}>
      {inUse ? (
        <div role="alert" className="flex flex-col gap-2 text-sm">
          <p>These rules still send to it:</p>
          <ul className="list-disc pl-5 font-medium">
            {inUse.map((n) => (
              <li key={n}>{n}</li>
            ))}
          </ul>
          <p className="text-muted">Remove the channel from them first.</p>
        </div>
      ) : (
        <p className="text-sm text-muted">Its secret and delivery history are erased.</p>
      )}
      {error && (
        <p role="alert" className="text-sm text-rose-700 dark:text-rose-300">
          {error}
        </p>
      )}
      <DialogActions>
        <Button variant="ghost" onClick={close}>
          {inUse ? "Close" : "Cancel"}
        </Button>
        {!inUse && (
          <Button variant="danger" disabled={del.isPending} onClick={() => del.mutate()}>
            Delete
          </Button>
        )}
      </DialogActions>
    </Dialog>
  );
}

const STATUSES: { value: DeliveryStatus | ""; label: string }[] = [
  { value: "", label: "Any status" },
  ...(Object.entries(DELIVERY_STATUS) as [DeliveryStatus, { label: string }][]).map(
    ([value, s]) => ({ value, label: s.label }),
  ),
];

function DeliveriesSection({ channels }: { channels: Channel[] }) {
  const [channelId, setChannelId] = useState("");
  const [status, setStatus] = useState<DeliveryStatus | "">("");
  const filters: DeliveryFilters = {
    channel_id: channelId || undefined,
    status: status || undefined,
  };
  const query = useInfiniteQuery({
    queryKey: ["deliveries", filters],
    queryFn: ({ pageParam }) => listDeliveries(filters, pageParam),
    initialPageParam: "",
    getNextPageParam: (p) => p.next_cursor,
    refetchInterval: 15_000,
  });
  const items = query.data?.pages.flatMap((p) => p.items) ?? [];
  const ids = { channel: useId(), status: useId() };
  return (
    <section className={sectionClass} aria-labelledby="deliveries-title">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="flex flex-col gap-1">
          <h2 id="deliveries-title" className="font-display text-xl font-semibold tracking-tight">
            Deliveries
          </h2>
          <p className="text-sm text-muted">
            Failed sends are tried up to 5 times with growing delays. A refusal that cannot succeed
            is not retried, but you can retry it by hand.
          </p>
        </div>
        <div className="flex max-w-full flex-wrap gap-3">
          <div className="flex max-w-full min-w-0 flex-col gap-1">
            <label htmlFor={ids.channel} className="text-xs font-medium text-muted">
              Channel
            </label>
            <select
              id={ids.channel}
              value={channelId}
              onChange={(e) => setChannelId(e.target.value)}
              className={cn(inputClass, "w-auto max-w-full pr-8")}
            >
              <option value="">All channels</option>
              {channels.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
          </div>
          <div className="flex flex-col gap-1">
            <label htmlFor={ids.status} className="text-xs font-medium text-muted">
              Status
            </label>
            <select
              id={ids.status}
              value={status}
              onChange={(e) => setStatus(e.target.value as DeliveryStatus | "")}
              className={cn(inputClass, "w-auto pr-8")}
            >
              {STATUSES.map((s) => (
                <option key={s.value} value={s.value}>
                  {s.label}
                </option>
              ))}
            </select>
          </div>
        </div>
      </div>
      {query.isError && !query.data ? (
        <ErrorState
          title="Deliveries could not be loaded."
          error={query.error}
          onRetry={() => void query.refetch()}
        />
      ) : !query.isPending && items.length === 0 ? (
        <p className="rounded-xl border border-dashed border-line px-6 py-8 text-sm text-muted">
          {channelId || status
            ? "No delivery matches these filters."
            : "No deliveries yet: they appear when a rule sends to a channel."}
        </p>
      ) : (
        <DeliveryTable items={items} loading={query.isPending} />
      )}
      {query.hasNextPage && (
        <Button
          variant="outline"
          className="self-center"
          disabled={query.isFetchingNextPage}
          onClick={() => void query.fetchNextPage()}
        >
          {query.isFetchingNextPage ? "Loading…" : "Load more"}
        </Button>
      )}
    </section>
  );
}

function DeliveryTable({ items, loading }: { items: Delivery[]; loading: boolean }) {
  const client = useQueryClient();
  const toast = useToast();
  const retry = useMutation({
    mutationFn: (id: number) => retryDelivery(id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["deliveries"] });
      toast("Delivery queued again");
    },
    onError: (err) => toast(`Could not retry: ${problemText(err)}`, "error"),
  });
  return (
    <div className="relative overflow-x-auto rounded-xl border border-line bg-surface">
      <table className="w-full min-w-[760px] text-left text-sm" aria-busy={loading}>
        <caption className="sr-only">Deliveries</caption>
        <thead className="border-b border-line text-xs tracking-wide text-muted uppercase">
          <tr>
            {["Status", "Event", "Channel", "Attempts", "Next attempt", "Error and logs"].map(
              (h) => (
                <th key={h} scope="col" className="px-4 py-3 font-medium">
                  {h}
                </th>
              ),
            )}
            <th scope="col" className="px-4 py-3">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {loading
            ? Array.from({ length: 3 }, (_, i) => (
                <tr
                  key={i}
                  data-testid="delivery-skeleton"
                  className="border-b border-line last:border-0"
                >
                  {Array.from({ length: 7 }, (_, j) => (
                    <td key={j} className="px-4 py-3.5">
                      <span
                        aria-hidden
                        className="block h-3.5 w-16 animate-pulse rounded bg-line"
                      />
                    </td>
                  ))}
                </tr>
              ))
            : items.map((d) => {
                const s = summarizeEvent(d.event);
                return (
                  <tr key={d.id} className="border-b border-line align-top last:border-0">
                    <td className="px-4 py-3">
                      <DeliveryBadge status={d.status} />
                    </td>
                    <td className="max-w-56 px-4 py-3">
                      <span className="block truncate">{s.text}</span>
                      <span className="block truncate text-xs text-muted">
                        {d.watch.name}
                        {d.rule ? ` · ${d.rule.name}` : ""} · <Timestamp at={d.created_at} />
                      </span>
                    </td>
                    <td className="px-4 py-3 whitespace-nowrap">
                      <span className="inline-flex items-center gap-1.5">
                        <ChannelIcon type={d.channel.type} className="shrink-0 text-muted" />
                        {d.channel.name}
                      </span>
                    </td>
                    <td className="px-4 py-3 tabular-nums">{d.attempts} / 5</td>
                    <td className="px-4 py-3 text-xs whitespace-nowrap text-muted">
                      {d.next_attempt_at ? formatRelative(new Date(d.next_attempt_at)) : "—"}
                    </td>
                    <td className="max-w-52 px-4 py-3">
                      {d.logs != null && <DeliveryLogs logs={d.logs} />}
                      {d.last_error ? (
                        <details>
                          <summary
                            className={cn(
                              "cursor-pointer truncate font-mono text-xs marker:text-muted",
                              // 012: a skipped delivery's reason is not a failure.
                              d.status === "skipped"
                                ? "text-muted"
                                : "text-rose-700 dark:text-rose-300",
                            )}
                          >
                            {d.last_error.split("\n")[0]}
                          </summary>
                          <pre className="mt-2 max-h-48 overflow-auto rounded-lg bg-bg p-2 font-mono text-xs break-words whitespace-pre-wrap">
                            {d.last_error}
                          </pre>
                        </details>
                      ) : (
                        d.logs == null && <span className="text-muted">—</span>
                      )}
                    </td>
                    <td className="px-4 py-2 text-right whitespace-nowrap">
                      {(d.status === "failed" || d.status === "dead") && (
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={retry.isPending}
                          onClick={() => retry.mutate(d.id)}
                        >
                          <RotateCw aria-hidden />
                          Retry
                        </Button>
                      )}
                    </td>
                  </tr>
                );
              })}
        </tbody>
      </table>
    </div>
  );
}

/** A script delivery's log lines, folded. */
export function DeliveryLogs({ logs }: { logs: string }) {
  const lines = logs ? logs.split("\n") : [];
  return (
    <details className="mb-1">
      <summary className="cursor-pointer text-xs text-muted marker:text-muted">
        Logs · {lines.length} line{lines.length === 1 ? "" : "s"}
      </summary>
      <pre className="mt-2 max-h-48 overflow-auto rounded-lg bg-bg p-2 font-mono text-xs break-words whitespace-pre-wrap">
        {logs || "No log line."}
      </pre>
    </details>
  );
}
