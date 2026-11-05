import { useQuery } from "@tanstack/react-query";

import { getFilterReference } from "@/api/client";

const code = "rounded bg-bg px-1 font-mono";

/** What a filter may use, as the server enforces it (GET /api/v1/filters/reference). */
export function FilterReference() {
  const q = useQuery({
    queryKey: ["filter-reference"],
    queryFn: getFilterReference,
    staleTime: Infinity,
  });
  if (q.isPending) return <p className="text-xs text-muted">Loading what filters may use…</p>;
  if (q.isError) {
    return (
      <p className="text-xs text-muted">
        The list of what filters may use is unavailable; saving still checks the filter.
      </p>
    );
  }
  const r = q.data;
  const l = r.limits;
  return (
    <section aria-label="Filter reference" className="flex flex-col gap-1.5 text-xs text-muted">
      <p>
        Operators: <code className={code}>{(r.operators ?? []).join(" ")}</code>
      </p>
      <p>
        Functions: <code className={code}>{(r.builtins ?? []).join(" ")}</code>
      </p>
      <p>
        Over an array: <code className={code}>{(r.predicates ?? []).join(" ")}</code>, e.g.{" "}
        <code className={code}>{'any(event.payload.items, {# startsWith "v"})'}</code>
      </p>
      <ul className="list-disc pl-5">
        {(r.rules ?? []).map((rule) => (
          <li key={rule}>{rule}</li>
        ))}
        <li>
          At most {l.max_length} characters, {l.max_nodes} expression nodes and a cost of{" "}
          {r.cost.budget.toLocaleString("en")} units (a compile error gives the computed cost);
          filters see payload strings up to {l.max_string} bytes ({l.max_body} for{" "}
          <code className={code}>event.payload.body</code>) and arrays up to {l.max_items} items.
          Empty matches every event.
        </li>
      </ul>
    </section>
  );
}
