import { Plus, Trash2 } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Field, inputClass } from "@/components/ui/field";
import { MAX_DIGEST_TIMES, timeZones } from "@/lib/channels";
import { cn } from "@/lib/utils";

/** 012: a rule's immediate or digest delivery, and the digest's daily times and time zone; locked names why it is off. */
export function DeliveryFields({
  locked,
  digest,
  setDigest,
  times,
  setTimes,
  tz,
  setTz,
  errors,
}: {
  locked?: string;
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
    <fieldset className="flex flex-col gap-2" disabled={Boolean(locked)}>
      <legend className="mb-1.5 text-sm font-medium">Delivery</legend>
      {[
        { on: false, label: "Immediate", hint: "One message per event, as it comes." },
        {
          on: true,
          label: "Digest",
          hint: "One message at set times, listing the events still unread.",
        },
      ].map((o) => (
        <label key={o.label} className="flex items-start gap-2 text-sm">
          <input
            type="radio"
            name="delivery"
            checked={(digest && !locked) === o.on}
            aria-describedby={errors.delivery || locked ? deliveryErrorId : undefined}
            onChange={() => setDigest(o.on)}
            className="mt-0.5 accent-beacon"
          />
          <span>
            {o.label} <span className="text-xs text-muted">{o.hint}</span>
          </span>
        </label>
      ))}
      {locked ? (
        <p id={deliveryErrorId} className="text-sm text-muted">
          {locked}
        </p>
      ) : (
        errors.delivery && (
          <p id={deliveryErrorId} className="text-sm text-rose-700 dark:text-rose-300">
            {errors.delivery}
          </p>
        )
      )}
      {digest && !locked && (
        <div className="flex flex-col gap-3 pl-6">
          <p className="text-xs text-muted">
            Webhook and script channels still get each event at once: they have no digest format.
          </p>
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
