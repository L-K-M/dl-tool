import { expect, test } from "vitest";
import { readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { initI18n, NAMESPACES, type Namespace } from "./i18n";

const localesDir = join(dirname(fileURLToPath(import.meta.url)), "locales");

function catalogue(name: string): Record<string, unknown> {
  return JSON.parse(
    readFileSync(join(localesDir, "en", `${name}.json`), "utf8"),
  ) as Record<string, unknown>;
}

function* leafValues(
  node: unknown,
  path: string,
): Generator<[string, unknown]> {
  if (node !== null && typeof node === "object") {
    for (const [key, value] of Object.entries(node)) {
      yield* leafValues(value, `${path}.${key}`);
    }
    return;
  }
  yield [path, node];
}

test("TestM3CataloguesExist", () => {
  for (const name of ["common", "grid", "dialogs", "errors"]) {
    expect(NAMESPACES).toContain(name);
    expect(typeof catalogue(name)).toBe("object");
  }

  // Every catalogue on disk is a reserved namespace; en is the only locale.
  const files = readdirSync(join(localesDir, "en")).filter((f) =>
    f.endsWith(".json"),
  );
  expect(files.length).toBeGreaterThan(0);
  for (const file of files) {
    expect(NAMESPACES).toContain(file.slice(0, -".json".length) as Namespace);
  }
  expect(readdirSync(localesDir)).toEqual(["en"]);

  const t = initI18n().t;
  expect(t("errors:problem.path-rejected")).toBe(
    "That folder is outside the allowed download roots.",
  );
});

test("TestNoEmptyCatalogueValues", () => {
  for (const file of readdirSync(join(localesDir, "en"))) {
    const data = JSON.parse(
      readFileSync(join(localesDir, "en", file), "utf8"),
    ) as Record<string, unknown>;
    expect(Object.keys(data).length).toBeGreaterThan(0);
    for (const [path, value] of leafValues(data, file)) {
      expect(value, path).not.toBe("");
    }
  }
});

test("TestPluralKeysResolve", () => {
  const t = initI18n().t;
  expect(t("errors:event.task.created", { count: 1 })).toBe("1 task created");
  expect(t("errors:event.task.created", { count: 2 })).toBe("2 tasks created");
});

test("TestMissingKeyThrowsInTests", () => {
  const i18n = initI18n({ throwOnMissing: true });
  expect(() => i18n.t("common:no.such.key")).toThrow(/missing i18n key/);

  // Neither a bare call nor an empty options object may clear strict mode.
  initI18n();
  initI18n({});
  expect(() => i18n.t("common:no.such.key")).toThrow(/missing i18n key/);

  initI18n({ throwOnMissing: false });
  expect(i18n.t("common:no.such.key")).toBe("no.such.key");
});
