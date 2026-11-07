import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { Dialog } from "@/components/ui/dialog";

// React bubbles cancel and close through the component tree: a nested dialog's own events stay its own.
describe("nested Dialog", () => {
  function renderNested(dismissible = false) {
    const parentClose = vi.fn();
    const childClose = vi.fn();
    render(
      <Dialog open title="Parent" dismissible={dismissible} onClose={parentClose}>
        <Dialog open title="Child" onClose={childClose}>
          <p>inside</p>
        </Dialog>
      </Dialog>,
    );
    return {
      parent: screen.getByRole("dialog", { name: "Parent" }),
      child: screen.getByRole("dialog", { name: "Child" }),
      parentClose,
      childClose,
    };
  }

  it("lets a child's Escape through a parent that is not dismissible", () => {
    const { parent, child } = renderNested();
    const cancel = new Event("cancel", { cancelable: true });
    child.dispatchEvent(cancel);
    expect(cancel.defaultPrevented).toBe(false);
    expect(parent).toHaveAttribute("open");
  });

  it("closes only the child when the child closes", () => {
    const { parent, child, parentClose, childClose } = renderNested(true);
    child.dispatchEvent(new Event("close"));
    expect(childClose).toHaveBeenCalledTimes(1);
    expect(parentClose).not.toHaveBeenCalled();
    expect(parent).toHaveAttribute("open");
  });

  it("still holds a parent that is not dismissible on its own Escape", () => {
    const { parent } = renderNested();
    const cancel = new Event("cancel", { cancelable: true });
    parent.dispatchEvent(cancel);
    expect(cancel.defaultPrevented).toBe(true);
  });
});
