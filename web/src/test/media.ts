type Listener = () => void;

let matches = new Map<string, boolean>();
let listeners = new Map<string, Set<Listener>>();

export function resetMedia(): void {
  matches = new Map();
  listeners = new Map();
  window.matchMedia = (query: string) =>
    ({
      get matches() {
        return matches.get(query) ?? false;
      },
      media: query,
      addEventListener: (_: string, l: Listener) => {
        const set = listeners.get(query) ?? new Set<Listener>();
        set.add(l);
        listeners.set(query, set);
      },
      removeEventListener: (_: string, l: Listener) => listeners.get(query)?.delete(l),
    }) as unknown as MediaQueryList;
}

export function setMedia(query: string, value: boolean): void {
  matches.set(query, value);
  listeners.get(query)?.forEach((l) => l());
}
