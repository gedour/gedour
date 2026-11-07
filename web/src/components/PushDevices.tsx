import { useMutation, useQueryClient } from "@tanstack/react-query";
import { BellPlus, Pencil, Trash2 } from "lucide-react";
import { useState, type FormEvent } from "react";

import { deletePushDevice, renamePushDevice } from "@/api/client";
import { ErrorState } from "@/components/ErrorState";
import { Timestamp } from "@/components/Timestamp";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { Field, inputClass } from "@/components/ui/field";
import { useToast } from "@/hooks/toast";
import { DEVICES_KEY, useAddBrowser, usePushDevices, usePushUnavailable } from "@/hooks/usePush";
import { problemText } from "@/lib/problems";
import { defaultDeviceName } from "@/lib/push";
import type { PushDevice } from "@/types/api";

const alertClass = "text-sm text-rose-700 dark:text-rose-300";

/** Web Push devices: this browser's registration, rename and removal (010-AC14, 010-AC15). */
export function PushDevicesSection() {
  const { devices, thisId } = usePushDevices();
  const unavailable = usePushUnavailable();
  const [name, setName] = useState(defaultDeviceName);
  const { add, error, busy } = useAddBrowser();
  const [renaming, setRenaming] = useState<PushDevice | null>(null);
  const [removing, setRemoving] = useState<PushDevice | null>(null);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    add(name.trim() || defaultDeviceName());
  };
  return (
    <section className="flex flex-col gap-4" aria-labelledby="push-devices-title">
      <div className="flex flex-col gap-1">
        <h2 id="push-devices-title" className="font-display text-xl font-semibold tracking-tight">
          Push devices
        </h2>
        <p className="max-w-prose text-sm text-muted">
          Browsers where Web Push channels show notifications. Add each browser from itself.
        </p>
      </div>
      {unavailable ? (
        <p className="rounded-lg bg-amber-500/10 px-3 py-2 text-sm text-amber-900 dark:text-amber-200">
          {unavailable}
        </p>
      ) : (
        thisId === null && (
          <form
            onSubmit={submit}
            aria-label="Add this browser"
            className="flex flex-wrap items-end gap-2"
          >
            <Field label="Name of this browser" className="min-w-0 flex-1 sm:max-w-xs">
              {(p) => (
                <input
                  {...p}
                  value={name}
                  maxLength={100}
                  onChange={(e) => setName(e.target.value)}
                  className={inputClass}
                />
              )}
            </Field>
            <Button type="submit" variant="outline" disabled={busy}>
              <BellPlus aria-hidden />
              {busy ? "Adding…" : "Add this browser"}
            </Button>
          </form>
        )
      )}
      {error && (
        <p role="alert" className={alertClass}>
          {error}
        </p>
      )}
      {devices.isError && !devices.data ? (
        <ErrorState
          title="Devices could not be loaded."
          error={devices.error}
          onRetry={() => void devices.refetch()}
        />
      ) : devices.data?.length === 0 ? (
        <p className="rounded-xl border border-dashed border-line px-6 py-6 text-sm text-muted">
          No device yet.
        </p>
      ) : (
        devices.data && (
          <ul
            aria-label="Push devices"
            className="flex flex-col divide-y divide-line rounded-xl border border-line bg-surface"
          >
            {devices.data.map((d) => (
              <li key={d.id} className="flex flex-wrap items-center gap-3 px-4 py-3">
                <div className="flex min-w-0 flex-1 flex-col gap-0.5">
                  <span className="flex items-center gap-2 font-medium">
                    <span className="truncate">{d.name}</span>
                    {d.id === thisId && (
                      <span className="shrink-0 rounded-md bg-beacon-soft px-1.5 py-0.5 text-xs font-medium text-ink ring-1 ring-beacon/25 ring-inset">
                        This browser
                      </span>
                    )}
                  </span>
                  <span className="truncate text-xs text-muted" title={d.user_agent}>
                    {d.user_agent || "Unknown browser"}
                  </span>
                  <span className="text-xs text-muted">
                    {d.last_push_at ? (
                      <>
                        Last push <Timestamp at={d.last_push_at} />
                      </>
                    ) : (
                      "No push yet"
                    )}
                  </span>
                </div>
                <span className="flex gap-1">
                  <Button
                    variant="ghost"
                    size="sm"
                    aria-label={`Rename ${d.name}`}
                    onClick={() => setRenaming(d)}
                  >
                    <Pencil aria-hidden />
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    aria-label={`Remove ${d.name}`}
                    onClick={() => setRemoving(d)}
                  >
                    <Trash2 aria-hidden />
                  </Button>
                </span>
              </li>
            ))}
          </ul>
        )
      )}
      <Dialog
        open={renaming !== null}
        title={`Rename ${renaming?.name ?? ""}`}
        onClose={() => setRenaming(null)}
      >
        {renaming && <RenameForm device={renaming} onClose={() => setRenaming(null)} />}
      </Dialog>
      <RemoveDialog device={removing} onClose={() => setRemoving(null)} />
    </section>
  );
}

/** Confirms a removal, as channel deletion does. */
function RemoveDialog({ device, onClose }: { device: PushDevice | null; onClose: () => void }) {
  const client = useQueryClient();
  const toast = useToast();
  const remove = useMutation({
    mutationFn: (d: PushDevice) => deletePushDevice(d.id),
    onSuccess: (_, d) => {
      void client.invalidateQueries({ queryKey: DEVICES_KEY });
      toast(`Removed ${d.name}`);
      close();
    },
  });
  const close = () => {
    remove.reset();
    onClose();
  };
  return (
    <Dialog open={device !== null} title={`Remove ${device?.name ?? ""}?`} onClose={close}>
      <p className="text-sm text-muted">
        Its channels stop notifying it. Add it again from that browser to undo.
      </p>
      {remove.isError && (
        <p role="alert" className={alertClass}>
          Could not remove the device: {problemText(remove.error)}
        </p>
      )}
      <DialogActions>
        <Button variant="ghost" onClick={close}>
          Cancel
        </Button>
        <Button
          variant="danger"
          disabled={remove.isPending}
          onClick={() => device && remove.mutate(device)}
        >
          Remove
        </Button>
      </DialogActions>
    </Dialog>
  );
}

function RenameForm({ device, onClose }: { device: PushDevice; onClose: () => void }) {
  const [name, setName] = useState(device.name);
  const client = useQueryClient();
  const save = useMutation({
    mutationFn: () => renamePushDevice(device.id, name),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: DEVICES_KEY });
      onClose();
    },
  });
  return (
    <form
      noValidate
      aria-label="Rename device"
      className="flex flex-col gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <Field label="Name" error={save.isError ? problemText(save.error) : undefined}>
        {(p) => (
          <input
            {...p}
            value={name}
            maxLength={100}
            onChange={(e) => setName(e.target.value)}
            className={inputClass}
          />
        )}
      </Field>
      <DialogActions>
        <Button type="button" variant="ghost" onClick={onClose}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={save.isPending}>
          {save.isPending ? "Saving…" : "Rename"}
        </Button>
      </DialogActions>
    </form>
  );
}
