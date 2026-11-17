import { Plus, Trash2 } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Field, inputClass } from "@/components/ui/field";
import { EVERY_DAY, MAX_DIGEST_TIMES, timeZones, WEEKDAYS, type DigestTime } from "@/lib/channels";
import { cn } from "@/lib/utils";

/** 012, 018: a rule's immediate or digest delivery, and the digest's times, their weekdays and time zone; locked names why it is off. */
export function DeliveryFields({
  locked,
  digest,
  setDigest,
  schedule,
  setSchedule,
  tz,
  setTz,
  errors,
}: {
  locked?: string;
  digest: boolean;
  setDigest: (v: boolean) => void;
  schedule: DigestTime[];
  setSchedule: (v: DigestTime[]) => void;
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
  }, [schedule]);
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
            {schedule.map(({ time, days }, i) => {
              const set = (e: DigestTime) => setSchedule(schedule.map((x, j) => (j === i ? e : x)));
              const timeError = errors[`schedule[${i}].time`];
              // 018-AC11: an empty row says so as soon as its last day goes, not only on save.
              const daysError =
                errors[`schedule[${i}].days`] ??
                (days.length === 0 ? "at least one day" : undefined);
              const timeErrorId = `${id}-time-${i}`;
              const daysErrorId = `${id}-days-${i}`;
              return (
                <li key={i} className="flex flex-wrap items-center gap-2">
                  <input
                    type="time"
                    aria-label={`Time ${i + 1}`}
                    aria-invalid={timeError ? true : undefined}
                    aria-describedby={timeError ? timeErrorId : undefined}
                    value={time}
                    onChange={(e) => set({ time: e.target.value, days })}
                    className={cn(inputClass, "w-32 font-mono")}
                  />
                  <div
                    role="group"
                    aria-label={`Days, time ${i + 1}`}
                    aria-describedby={daysError ? daysErrorId : undefined}
                    className="flex flex-wrap gap-1"
                  >
                    {WEEKDAYS.map((d) => {
                      const on = days.includes(d.id);
                      return (
                        <Button
                          key={d.id}
                          type="button"
                          variant="outline"
                          size="sm"
                          aria-label={`${d.label}, time ${i + 1}`}
                          aria-pressed={on}
                          onClick={() =>
                            set({
                              time,
                              // Rebuilt from the week so days stay Monday-first, as the API returns them.
                              days: EVERY_DAY.filter((x) => (x === d.id ? !on : days.includes(x))),
                            })
                          }
                          className={cn(
                            "w-8 px-0",
                            on ? "border-beacon bg-beacon-soft hover:bg-beacon-soft" : "text-muted",
                          )}
                        >
                          {d.short}
                        </Button>
                      );
                    })}
                  </div>
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    aria-label={`Remove time ${i + 1}`}
                    disabled={schedule.length === 1}
                    onClick={() => {
                      focusRow.current = i - 1;
                      setSchedule(schedule.filter((_, j) => j !== i));
                    }}
                  >
                    <Trash2 aria-hidden />
                  </Button>
                  {timeError && (
                    <p id={timeErrorId} className="w-full text-sm text-rose-700 dark:text-rose-300">
                      {timeError}
                    </p>
                  )}
                  {/* Mounted before the error so the live region announces it; out of the flow while empty. */}
                  <div aria-live="polite" className="w-full empty:sr-only">
                    {daysError && (
                      <p id={daysErrorId} className="text-sm text-rose-700 dark:text-rose-300">
                        {daysError}
                      </p>
                    )}
                  </div>
                </li>
              );
            })}
          </ul>
          {errors.schedule && (
            <p className="text-sm text-rose-700 dark:text-rose-300">{errors.schedule}</p>
          )}
          <Button
            ref={addButton}
            type="button"
            variant="outline"
            size="sm"
            className="self-start"
            disabled={schedule.length >= MAX_DIGEST_TIMES}
            onClick={() => {
              focusRow.current = schedule.length;
              setSchedule([...schedule, { time: "", days: EVERY_DAY }]);
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
