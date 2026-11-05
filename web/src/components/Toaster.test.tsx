import { act, render, renderHook, screen } from "@testing-library/react";
import { useEffect } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "@/components/Toaster";
import { useToast } from "@/hooks/toast";

function Push({ message }: { message: string }) {
  const toast = useToast();
  useEffect(() => toast(message), [toast, message]);
  return null;
}

describe("toasts", () => {
  afterEach(() => vi.useRealTimers());

  it("is a no-op outside the shell's provider", () => {
    const { result } = renderHook(() => useToast());
    expect(() => result.current("hello", "error")).not.toThrow();
  });

  it("goes away on its own after 5 s", () => {
    vi.useFakeTimers();
    render(
      <ToastProvider>
        <Push message="Saved" />
      </ToastProvider>,
    );
    expect(screen.getByText("Saved")).toBeInTheDocument();
    act(() => vi.advanceTimersByTime(4999));
    expect(screen.getByText("Saved")).toBeInTheDocument();
    act(() => vi.advanceTimersByTime(1));
    expect(screen.queryByText("Saved")).toBeNull();
  });
});
