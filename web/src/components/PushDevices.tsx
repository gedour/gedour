import { useMutation, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { Pencil, Trash2 } from "lucide-react";
import { useState } from "react";

import { deletePushDevice, renamePushDevice } from "@/api/client";
import { ErrorState } from "@/components/ErrorState";
import { Timestamp } from "@/components/Timestamp";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { Field, inputClass } from "@/components/ui/field";
import { useToast } from "@/hooks/toast";
import { DEVICES_KEY } from "@/hooks/usePush";
import { problemText } from "@/lib/problems";
import type { PushDevice } from "@/types/api";

const alertClass = "text-sm text-rose-700 dark:text-rose-300";

/**
 * The user's devices in the Web Push channel form (010-AC18): rename, remove, and a checkbox each while
 * picked is a list.
 */
export function PushDeviceList({
  devices,
  here,
  picked,
  onPick,
  onRename,
  onRemove,
  blocked,
}: {
  devices: UseQueryResult<PushDevice[]>;
  here: string | null | undefined;
  picked: string[] | null;
  onPick: (ids: string[]) => void;
  onRename: (d: PushDevice) => void;
  onRemove: (d: PushDevice) => void;
  /** This browser cannot be added (no Push API, denied, Web Push off): the form says why. */
  blocked: boolean;
}) {
  if (devices.isError && !devices.data)
    return (
      <ErrorState
        title="Devices could not be loaded."
        error={devices.error}
        onRetry={() => void devices.refetch()}
      />
    );
  if (!devices.data) return null;
  if (devices.data.length === 0)
    return (
      <p className="text-sm text-muted">
        {blocked
          ? "No device yet."
          : "No device yet: add this browser, or open this form in another browser to add that one."}
      </p>
    );
  return (
    <ul
      aria-label="My devices"
      className="flex flex-col divide-y divide-line rounded-lg border border-line"
    >
      {devices.data.map((d) => {
        const name = (
          <span className="flex min-w-0 items-center gap-2 font-medium">
            <span className="min-w-0 wrap-anywhere">{d.name}</span>
            {d.id === here && (
              <span className="shrink-0 rounded-md bg-beacon-soft px-1.5 py-0.5 text-xs font-medium text-ink ring-1 ring-beacon/25 ring-inset">
                This browser
              </span>
            )}
          </span>
        );
        return (
          <li key={d.id} className="flex items-center gap-3 px-3 py-2">
            <div className="flex min-w-0 flex-1 flex-col gap-0.5 text-sm">
              {picked ? (
                <label className="flex items-center gap-2">
                  <input
                    type="checkbox"
                    checked={picked.includes(d.id)}
                    onChange={(e) =>
                      onPick(
                        e.target.checked ? [...picked, d.id] : picked.filter((x) => x !== d.id),
                      )
                    }
                    className="accent-beacon"
                  />
                  {name}
                </label>
              ) : (
                name
              )}
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
            <span className="flex shrink-0 gap-1">
              <Button
                type="button"
                variant="ghost"
                size="sm"
                aria-label={`Rename ${d.name}`}
                data-rename={d.id}
                onClick={() => onRename(d)}
              >
                <Pencil aria-hidden />
              </Button>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                aria-label={`Remove ${d.name}`}
                onClick={() => onRemove(d)}
              >
                <Trash2 aria-hidden />
              </Button>
            </span>
          </li>
        );
      })}
    </ul>
  );
}

/** Rename and remove dialogs for PushDeviceList; render them outside the channel form, forms do not nest. */
export function PushDeviceDialogs({
  renaming,
  removing,
  onClose,
  onRemoved,
}: {
  renaming: PushDevice | null;
  removing: PushDevice | null;
  onClose: () => void;
  onRemoved: (d: PushDevice) => void;
}) {
  return (
    <>
      <Dialog open={renaming !== null} title={`Rename ${renaming?.name ?? ""}`} onClose={onClose}>
        {renaming && <RenameForm device={renaming} onClose={onClose} />}
      </Dialog>
      <RemoveDialog device={removing} onRemoved={onRemoved} onClose={onClose} />
    </>
  );
}

/** Confirms a removal, as channel deletion does. */
function RemoveDialog({
  device,
  onRemoved,
  onClose,
}: {
  device: PushDevice | null;
  onRemoved: (d: PushDevice) => void;
  onClose: () => void;
}) {
  const client = useQueryClient();
  const toast = useToast();
  const remove = useMutation({
    mutationFn: (d: PushDevice) => deletePushDevice(d.id),
    onSuccess: (_, d) => {
      // Dropped at once: the form moves the focus off the row even if the refetch fails.
      client.setQueryData<PushDevice[]>(DEVICES_KEY, (old) => old?.filter((x) => x.id !== d.id));
      void client.invalidateQueries({ queryKey: DEVICES_KEY });
      toast(`Removed ${d.name}`);
      onRemoved(d);
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
