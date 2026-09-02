import { render, screen } from "@testing-library/react";
import { createElement } from "react";
import { expect, test } from "vitest";

import { App } from "./main";

test("renders the application root", () => {
  render(createElement(App));

  expect(screen.getByTestId("app-root").textContent).toBe("dl-tool");
});
