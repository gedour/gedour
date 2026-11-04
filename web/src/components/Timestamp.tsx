import { Tooltip } from "@/components/ui/tooltip";
import { formatAbsolute, formatRelative } from "@/lib/time";

/** Relative time, the absolute one in a tooltip and the accessible name. */
export function Timestamp({ at, now }: { at: string; now?: Date }) {
  const date = new Date(at);
  const absolute = formatAbsolute(date);
  return (
    <Tooltip label={absolute} side="bottom">
      <time
        dateTime={at}
        tabIndex={0}
        aria-label={absolute}
        className="cursor-default rounded tabular-nums"
      >
        {formatRelative(date, now)}
      </time>
    </Tooltip>
  );
}
