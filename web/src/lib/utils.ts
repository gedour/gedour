import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}

// Storage can throw (private mode, quota, disabled cookies); preferences then just don't persist.
export function readStorage(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

export function writeStorage(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    // ignored: see readStorage
  }
}

/** Two-letter avatar text: "Ada Lovelace" → "AL", "dev@local" → "DL". */
export function initials(name: string): string {
  const [first = "?", second] = name.split(/[\s@._-]+/).filter(Boolean);
  return (second ? first.charAt(0) + second.charAt(0) : first.slice(0, 2)).toUpperCase();
}
