export function Wordmark() {
  return (
    <span className="flex items-center gap-2 font-display text-lg font-semibold tracking-tight text-white">
      <svg viewBox="0 0 32 32" className="size-6" aria-hidden>
        <circle cx="11" cy="21" r="3.5" className="fill-harbor-beacon" />
        <path
          d="M11 12.5a8.5 8.5 0 0 1 8.5 8.5M11 6a15 15 0 0 1 15 15"
          fill="none"
          className="stroke-harbor-beacon"
          strokeWidth="2.6"
          strokeLinecap="round"
        />
      </svg>
      gedour
    </span>
  );
}
