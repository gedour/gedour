import { EmptyState } from "@/components/EmptyState";
import type { NavItem } from "@/nav";

export function PlaceholderPage({ item }: { item: NavItem }) {
  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-8 px-5 py-8 md:px-10 md:py-12">
      <title>{`${item.label} – gedour`}</title>
      <h1 className="font-display text-3xl font-semibold tracking-tight">{item.label}</h1>
      <EmptyState title={item.emptyTitle} body={item.emptyBody} />
    </div>
  );
}
