import "@testing-library/jest-dom/vitest";

import { cleanup } from "@testing-library/react";
import { afterEach, beforeEach } from "vitest";

import { resetMedia } from "./media";

beforeEach(() => {
  localStorage.clear();
  resetMedia();
  document.documentElement.className = "";
});

afterEach(cleanup);

// jsdom lacks these; Radix primitives call them.
globalThis.ResizeObserver ??= class {
  observe() {}
  unobserve() {}
  disconnect() {}
};
Element.prototype.scrollIntoView ??= function () {};
Element.prototype.hasPointerCapture ??= () => false;
Element.prototype.releasePointerCapture ??= function () {};
