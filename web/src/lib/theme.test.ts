import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { createElement, type ReactElement } from "react";
import { cleanup, render, waitFor } from "@testing-library/react";
import { HttpResponse, http } from "msw";
import { setupServer } from "msw/node";
import {
  CircleCheckIcon,
  InfoIcon,
  TriangleAlertIcon,
  OctagonXIcon,
  Loader2Icon,
} from "lucide-react";
import type { ToasterProps } from "sonner";
import ts from "typescript";
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  expect,
  test,
  vi,
} from "vitest";
import { Toaster } from "../components/ui/sonner";
import { initI18n, NAMESPACES } from "../i18n";
import { defaultPrefs, useUiPrefs } from "../store/useUiPrefs";
import { applyTheme, readStoredTheme, resolveTheme, storeTheme } from "./theme";
import { cn } from "./utils";

const { captureToast } = vi.hoisted(() => ({ captureToast: vi.fn() }));
vi.mock("sonner", () => ({
  Toaster: (props: ToasterProps) => {
    captureToast(props);
    return null;
  },
}));

const webRoot = process.cwd();
const read = (path: string) => readFileSync(resolve(webRoot, path), "utf8");
const choices = ["light", "dark", "system"] as const;

const server = setupServer();
const putBodies: Record<string, unknown>[] = [];

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
beforeEach(() => {
  putBodies.length = 0;
  // The store is the document's only reader and writer now; reset its
  // members rather than a persisted row. Writes are gated on an applied
  // hydrate, so the seeded store is already hydrated.
  useUiPrefs.setState({
    ...JSON.parse(JSON.stringify(defaultPrefs)),
    hydrated: true,
  });
  server.use(
    http.get("*/api/v1/prefs", () => HttpResponse.json({})),
    http.put("*/api/v1/prefs", async ({ request }) => {
      putBodies.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json({});
    }),
  );
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  server.resetHandlers();
  document.documentElement.className = "";
  captureToast.mockClear();
});
afterAll(() => server.close());

test("TestResolveThemeFollowsSystem", () => {
  const media = vi.fn().mockReturnValue({ matches: true });
  vi.stubGlobal("matchMedia", media);
  expect(resolveTheme("system")).toBe("dark");
  media.mockReturnValue({ matches: false });
  expect(resolveTheme("system")).toBe("light");
  expect(media).toHaveBeenCalledWith("(prefers-color-scheme: dark)");
  for (const choice of ["light", "dark"] as const) {
    expect(resolveTheme(choice)).toBe(choice);
  }
});

test("TestApplyThemeTogglesClass", () => {
  document.documentElement.className = "unrelated";
  for (const choice of ["dark", "dark", "light", "light"] as const) {
    applyTheme(choice);
    expect(document.documentElement.classList.contains("dark")).toBe(
      choice === "dark",
    );
    expect(document.documentElement.classList.contains("unrelated")).toBe(true);
  }
});

test("TestReadStoredThemeReadsStore", () => {
  // The store renders defaultPrefs until a hydrate lands (doc 09 §3.3), so
  // the pre-paint read is "system" until the document arrives.
  expect(readStoredTheme()).toBe("system");
  for (const theme of ["dark", "light", "system"] as const) {
    useUiPrefs.setState({ theme });
    expect(readStoredTheme()).toBe(theme);
  }
});

test("TestStoreThemeWritesThemeMemberToDocument", async () => {
  useUiPrefs.setState({ sidebarWidth: 240, savedSearches: { keep: [1] } });
  storeTheme("dark");
  expect(readStoredTheme()).toBe("dark");
  // The debounced writer PUTs the whole document; the theme member and the
  // members other features own reach the server verbatim (doc 05 §11.4).
  await waitFor(() => expect(putBodies).toHaveLength(1));
  expect(putBodies[0]!.theme).toBe("dark");
  expect(putBodies[0]!.sidebarWidth).toBe(240);
  expect(putBodies[0]!.savedSearches).toEqual({ keep: [1] });
});

test("TestThemeAppliedBeforeRootCreation", async () => {
  vi.resetModules();
  // The pre-paint call reads the store, so a stored choice only reaches it
  // through the document the store already holds.
  const { useUiPrefs } = await import("../store/useUiPrefs");
  useUiPrefs.setState({ theme: "dark" });
  const host = document.createElement("div");
  host.id = "root";
  document.body.append(host);
  const createRoot = vi.fn(() => {
    expect(document.documentElement.classList.contains("dark")).toBe(true);
    return { render: vi.fn() };
  });
  vi.doMock("react-dom/client", () => ({ createRoot }));
  try {
    await import("../main");
    expect(createRoot).toHaveBeenCalledWith(host);
  } finally {
    host.remove();
    vi.doUnmock("react-dom/client");
  }
});

test("TestSonnerForwardsThemeChoice", () => {
  const view = render(
    createElement(Toaster, { theme: "light", position: "top-center" }),
  );
  for (const theme of choices) {
    view.rerender(createElement(Toaster, { theme, position: "top-center" }));
    expect(captureToast.mock.lastCall?.[0]).toMatchObject({
      theme,
      position: "top-center",
    });
  }
});

test("TestSonnerPreservesIcons", () => {
  render(createElement(Toaster, { theme: "system" }));
  const props: ToasterProps = captureToast.mock.lastCall![0];
  const expected = {
    success: CircleCheckIcon,
    info: InfoIcon,
    warning: TriangleAlertIcon,
    error: OctagonXIcon,
    loading: Loader2Icon,
  };
  expect(Object.keys(props.icons!)).toEqual(Object.keys(expected));
  for (const [slot, component] of Object.entries(expected)) {
    const icon = props.icons![slot as keyof typeof expected] as ReactElement<{
      className: string;
    }>;
    expect(icon.type).toBe(component);
    expect(icon.props).toEqual({
      className: slot === "loading" ? "size-4 animate-spin" : "size-4",
    });
  }
  expect(props.className).toBe("toaster group");
  expect(props.style).toEqual({
    "--normal-bg": "var(--popover)",
    "--normal-text": "var(--popover-foreground)",
    "--normal-border": "var(--border)",
    "--border-radius": "var(--radius)",
  });
  expect(props.toastOptions).toEqual({ classNames: { toast: "cn-toast" } });
});

test("TestBundledI18nAndClassMerging", () => {
  const i18n = initI18n();
  expect(initI18n()).toBe(i18n);
  expect(i18n.isInitialized).toBe(true);
  expect(i18n.language).toBe("en");
  expect(i18n.options.ns).toEqual(NAMESPACES);
  expect(i18n.t("appTitle")).toBe("dl-tool");
  for (const choice of choices)
    expect(i18n.t(`theme.${choice}`)).toBe(
      choice[0].toUpperCase() + choice.slice(1),
    );
  expect(
    i18n.t("empty.searchResults", { query: "ubuntu 26.04", count: 3 }),
  ).toBe('No results for "ubuntu 26.04" across 3 indexers.');
  expect(
    cn("px-2", false && "hidden", { block: true, inline: false }, ["px-4"]),
  ).toBe("block px-4");
});

// Keep the task scanner's exact normalization and rejection fixtures, not a host exemption.
function auditCSSURLs(css: string): void {
  const identifiers = new Set([
    "http://www.w3.org/2000/svg",
    "http://www.w3.org/1998/Math/MathML",
    "http://www.w3.org/1999/xlink",
    "http://www.w3.org/XML/1998/namespace",
    "https://react.dev/errors/",
  ]);
  const unexpectedURLs = (text: string) =>
    [...text.matchAll(/(?:https?:|(?<=[="'`(,]\s*))\/\/[^\s"'`<>\\)]+/gi)]
      .map((match) => match[0])
      .filter((url) => !identifiers.has(url));

  for (const identifier of identifiers) {
    assert.deepEqual(unexpectedURLs(`"${identifier}"`), []);
    const external = "https://cdn.example/app.js";
    assert.deepEqual(unexpectedURLs(`"${identifier}";"${external}"`), [
      external,
    ]);
    for (const suffix of ["/asset.js", "?asset=1", "#asset"]) {
      assert.deepEqual(unexpectedURLs(`"${identifier}${suffix}"`), [
        `${identifier}${suffix}`,
      ]);
    }
  }
  assert.deepEqual(
    unexpectedURLs(
      'document.createElementNS("http://www.w3.org/2000/svg", "svg")',
    ),
    [],
  );
  assert.deepEqual(unexpectedURLs('"HTTP://CDN.example/app.js"'), [
    "HTTP://CDN.example/app.js",
  ]);
  assert.deepEqual(unexpectedURLs("url(https://cdn.example/font.woff2)"), [
    "https://cdn.example/font.woff2",
  ]);
  assert.deepEqual(unexpectedURLs('"./assets/app.js"'), []);
  for (const external of [
    "//cdn.example/app.js",
    "//cdn/app.js",
    "//[::1]/app.js",
  ]) {
    assert.deepEqual(unexpectedURLs(`src="${external}"`), [external]);
    assert.deepEqual(unexpectedURLs(`url(${external})`), [external]);
    assert.deepEqual(
      unexpectedURLs(`"http://www.w3.org/2000/svg";"${external}"`),
      [external],
    );
    for (const space of ["", " ", "  ", "\t", "\n"]) {
      assert.deepEqual(unexpectedURLs(`url(${space}${external} )`), [external]);
      assert.deepEqual(
        unexpectedURLs(`srcset="a.png 1x,${space}${external} 2x"`),
        [external],
      );
      assert.deepEqual(unexpectedURLs(`src=${space}${external}`), [external]);
    }
  }
  assert.deepEqual(unexpectedURLs("\n//# sourceMappingURL=app.js.map"), []);
  const licenseComment =
    "/*! tailwindcss v4.3.3 | MIT License | https://tailwindcss.com */";
  const unexpectedAssetURLs = (file: string, text: string) =>
    unexpectedURLs(
      file.endsWith(".css") && text.startsWith(licenseComment)
        ? text.slice(licenseComment.length)
        : text,
    );
  assert.deepEqual(
    unexpectedAssetURLs("app.css", `${licenseComment}body{color:red}`),
    [],
  );
  for (const external of [
    "https://cdn.example/font.woff2",
    "https://tailwindcss.com",
    "//cdn.example/font.woff2",
  ]) {
    assert.deepEqual(
      unexpectedAssetURLs(
        "app.css",
        `${licenseComment}body{background:url(${external})}`,
      ),
      [external],
    );
  }
  for (const [file, text] of [
    [
      "app.css",
      licenseComment.replace("tailwindcss.com", "tailwindcss.com/asset"),
    ],
    ["app.css", `a{content:"${licenseComment}"}`],
    ["app.js", `const text = ${JSON.stringify(licenseComment)};`],
    ["index.html", licenseComment],
    ["app.css", `${licenseComment}${licenseComment}`],
  ]) {
    assert.ok(unexpectedAssetURLs(file, text).length > 0, `${file}: ${text}`);
  }
  expect(unexpectedAssetURLs("app.css", css)).toEqual([]);
  expect(css.startsWith(licenseComment)).toBe(true);
  // The installed compiler prepends a comment node, not a CSS resource reference.
  const compiler = read("node_modules/tailwindcss/dist/lib.mjs");
  expect(compiler).toContain("`! tailwindcss v${");
  expect(compiler).toContain(" | MIT License | https://tailwindcss.com ");
  console.log("CSS_URL_SCAN_REGRESSIONS_AND_LICENSE_AUDIT_OK");
}

const tokens = {
  bg: ["#f7f7f8", "#0f1115"],
  "bg-elevated": ["#ffffff", "#171a20"],
  fg: ["#14161a", "#e7e9ee"],
  "fg-muted": ["#5b6472", "#98a1b0"],
  border: ["#d8dbe0", "#2a2f38"],
  accent: ["#2563eb", "#60a5fa"],
  "accent-fg": ["#ffffff", "#0f1115"],
  ok: ["#15803d", "#4ade80"],
  warn: ["#b45309", "#fbbf24"],
  error: ["#b91c1c", "#f87171"],
  "progress-track": ["#e5e7eb", "#2a2f38"],
  "progress-fill": ["#2563eb", "#60a5fa"],
  "focus-ring": ["#2563eb", "#60a5fa"],
};
const aliases = {
  background: "bg",
  foreground: "fg",
  "card-foreground": "fg",
  "popover-foreground": "fg",
  "secondary-foreground": "fg",
  card: "bg-elevated",
  popover: "bg-elevated",
  secondary: "bg-elevated",
  muted: "bg-elevated",
  primary: "accent",
  "primary-foreground": "accent-fg",
  "accent-foreground": "accent-fg",
  "muted-foreground": "fg-muted",
  destructive: "error",
  input: "border",
  ring: "focus-ring",
};
const forbidden = [
  "next-themes",
  "react-dropzone",
  "react-hotkeys-hook",
  "parse-torrent",
  "d3-shape",
  "cmdk",
  "vaul",
  "cn",
];

type LockedPackage = {
  version?: string;
  dev?: boolean;
  dependencies?: Record<string, string>;
  optionalDependencies?: Record<string, string>;
  peerDependencies?: Record<string, string>;
};

function resolveLocked(
  packages: Record<string, LockedPackage>,
  from: string,
  name: string,
): string | undefined {
  let directory = from;
  for (;;) {
    const candidate = `${directory ? directory + "/" : ""}node_modules/${name}`;
    if (candidate in packages) return candidate;
    if (!directory) return undefined;
    const parent = dirname(directory);
    directory = parent === "." ? "" : parent;
  }
}

function sourceFiles(directory: string): string[] {
  return readdirSync(resolve(webRoot, directory), {
    withFileTypes: true,
  }).flatMap((entry) => {
    const path = `${directory}/${entry.name}`;
    return entry.isDirectory() ? sourceFiles(path) : [path];
  });
}

// Run installed Vite in Node, not Vitest's simulated browser. Both builds use the real config.
const buildAudit = String.raw`
import assert from 'node:assert/strict';
import { build, resolveConfig } from 'vite';
import { createRequire } from 'node:module';
import { resolve } from 'node:path';
import net from 'node:net';
// Both builds are offline; even a plugin attempting a request must fail.
net.Socket.prototype.connect = () => { throw new Error('Network forbidden during build audit'); };
globalThis.fetch = () => { throw new Error('Network forbidden during build audit'); };
const config = await resolveConfig({}, 'build');
assert.equal(config.base, './');
assert.equal(config.resolve.alias.find(a => a.find === '@').replacement, resolve('src'));
function audit(output) {
  const chunks = output.filter(item => item.type === 'chunk');
  assert.ok(chunks.length > 0);
  for (const chunk of chunks) {
    for (const id of Object.keys(chunk.modules)) {
      if (/\/node_modules\/(zod|shadcn)\//.test(id) && !id.split('?')[0].endsWith('.css')) {
        throw new Error('Tool JavaScript in browser chunk: ' + id);
      }
    }
  }
}
const result = await build({ build: { write: false }, logLevel: 'silent' });
assert.ok(!Array.isArray(result));
audit(result.output);
const css = result.output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n');
assert.ok(css.length > 0);
console.log('APPLICATION_BUILD_EXCLUDES_TOOL_JAVASCRIPT');
// A virtual entry uses the installed tool dependency without adding a source file or dependency.
const require = createRequire(import.meta.url);
const toolRequire = createRequire(require.resolve('shadcn'));
const zod = toolRequire.resolve('zod');
const fixture = '\0t039-runtime-validator';
const negative = await build({
  plugins: [{ name: 't039-negative-fixture', resolveId: id => id === fixture ? id : undefined,
    load: id => id === fixture ? 'import { z } from ' + JSON.stringify(zod) + '; console.log(z.string().parse("used"));' : undefined }],
  build: { write: false, rolldownOptions: { input: fixture } }, logLevel: 'silent'
});
assert.ok(!Array.isArray(negative));
assert.throws(() => audit(negative.output), /Tool JavaScript in browser chunk: .*\/zod\//);
console.log('RUNTIME_ZOD_IMPORT_REJECTED');
console.log(JSON.stringify({ css }));
`;

test("TestShadcnIntegrationContract", () => {
  const manifest = JSON.parse(read("package.json"));
  const pins = {
    react: "19.2.8",
    "react-dom": "19.2.8",
    vite: "8.2.2",
    "@vitejs/plugin-react": "6.1.1",
    typescript: "5.9.3",
    tailwindcss: "4.3.3",
    "@tailwindcss/vite": "4.3.3",
    shadcn: "4.19.1",
    "@tanstack/react-table": "8.21.3",
    "@tanstack/react-virtual": "3.14.10",
    "@tanstack/react-query": "5.102.8",
    zustand: "5.0.15",
    "react-router-dom": "7.18.3",
    i18next: "26.4.1",
    "react-i18next": "17.0.13",
    "lucide-react": "1.38.0",
    "tailwind-merge": "3.6.0",
    clsx: "2.1.1",
    "class-variance-authority": "0.7.1",
    "openapi-typescript": "7.13.0",
    "openapi-fetch": "0.17.0",
    vitest: "4.1.11",
    "@testing-library/react": "16.3.3",
    msw: "2.15.0",
    "@playwright/test": "1.62.1",
    eslint: "10.9.1",
    "typescript-eslint": "8.69.0",
    prettier: "3.9.6",
  };
  expect({
    ...manifest.dependencies,
    ...manifest.devDependencies,
  }).toMatchObject(pins);
  expect(manifest.devDependencies.shadcn).toBe(pins.shadcn);
  for (const name of [
    "@dnd-kit/core",
    "@dnd-kit/sortable",
    "react-resizable-panels",
    "radix-ui",
    "tw-animate-css",
    "sonner",
  ]) {
    expect(manifest.dependencies[name]).toMatch(/^\d+\.\d+\.\d+$/);
  }
  for (const section of [
    "dependencies",
    "devDependencies",
    "optionalDependencies",
    "peerDependencies",
  ]) {
    for (const name of [...forbidden, "zod"])
      expect(manifest[section] ?? {}).not.toHaveProperty(name);
  }
  const packages: Record<string, LockedPackage> = JSON.parse(
    read("package-lock.json"),
  ).packages;
  expect(packages["node_modules/shadcn"]).toMatchObject({
    version: pins.shadcn,
    dev: true,
  });
  const reachable = new Set<string>();
  const pending = ["node_modules/shadcn"];
  while (pending.length) {
    const path = pending.pop()!;
    if (reachable.has(path)) continue;
    reachable.add(path);
    const pkg = packages[path];
    for (const name of Object.keys({
      ...pkg.dependencies,
      ...pkg.optionalDependencies,
      ...pkg.peerDependencies,
    })) {
      const dependency = resolveLocked(packages, path, name);
      if (dependency) pending.push(dependency);
    }
  }
  let validators = 0;
  for (const [path, pkg] of Object.entries(packages)) {
    const name = path.split("node_modules/").at(-1)!;
    expect(forbidden).not.toContain(name);
    expect(name).not.toMatch(/^@fontsource/);
    if (name !== "zod") continue;
    validators++;
    expect(pkg.dev).toBe(true);
    expect(reachable.has(path)).toBe(true);
  }
  expect(validators).toBeGreaterThan(0);
  for (const path of sourceFiles("src").filter(
    (path) => !/\.test\.[^.]+$/.test(path),
  )) {
    const source = read(path);
    expect(source).not.toMatch(/@fontsource|@font-face/);
    if (!/\.[jt]sx?$/.test(path)) continue;
    const ast = ts.createSourceFile(path, source, ts.ScriptTarget.Latest, true);
    const imports: string[] = [];
    function visit(node: ts.Node): void {
      if (
        (ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) &&
        node.moduleSpecifier &&
        ts.isStringLiteral(node.moduleSpecifier)
      )
        imports.push(node.moduleSpecifier.text);
      if (
        ts.isCallExpression(node) &&
        (node.expression.kind === ts.SyntaxKind.ImportKeyword ||
          node.expression.getText(ast) === "require") &&
        node.arguments[0] &&
        ts.isStringLiteral(node.arguments[0])
      )
        imports.push(node.arguments[0].text);
      ts.forEachChild(node, visit);
    }
    visit(ast);
    for (const specifier of imports) {
      for (const banned of [...forbidden, "zod"])
        expect(specifier === banned || specifier.startsWith(`${banned}/`)).toBe(
          false,
        );
      if (specifier.startsWith("shadcn"))
        expect(specifier).toBe("shadcn/tailwind.css");
    }
  }
  const css = read("src/index.css");
  for (const [index, selector] of [":root", ".dark"].entries()) {
    const declarations = css.match(
      new RegExp(`(?:^|\\n)${selector.replace(".", "\\.")} \\{([^}]+)\\}`),
    )![1];
    for (const [name, values] of Object.entries(tokens))
      expect(declarations).toContain(`--${name}: ${values[index]};`);
  }
  const bridge = css.match(/:root,\s*\.dark\s*\{([^}]+)\}/)![1];
  const theme = css.match(/@theme inline\s*\{([^}]+)\}/)![1];
  for (const [name, token] of Object.entries(aliases)) {
    expect(bridge).toContain(`--${name}: var(--${token});`);
    expect(theme).toContain(`--color-${name}: var(--${name});`);
  }
  for (const name of ["border", "accent"])
    expect(theme).toContain(`--color-${name}: var(--${name});`);
  expect(css.match(/@import "([^"]+)"/g)).toEqual([
    '@import "tailwindcss"',
    '@import "tw-animate-css"',
    '@import "shadcn/tailwind.css"',
  ]);
  expect(css).toContain("@custom-variant dark (&:is(.dark *));");
  expect(css).toMatch(/--font-sans:\s+ui-sans-serif, system-ui, sans-serif/);
  expect(css).toContain("--font-heading: var(--font-sans)");
  expect(css).toContain("outline: 2px solid var(--focus-ring)");
  expect(css).toContain("outline-offset: 2px");
  expect(css).toMatch(
    /prefers-reduced-motion: reduce[\s\S]*animation: none !important;[\s\S]*transition: none !important;/,
  );
  expect(JSON.parse(read("tsconfig.json")).compilerOptions.paths).toEqual({
    "@/*": ["./src/*"],
  });
  expect(JSON.parse(read("components.json"))).toMatchObject({
    style: "radix-nova",
    tailwind: { css: "src/index.css" },
    aliases: { utils: "@/lib/utils" },
  });
  expect(readdirSync(resolve(webRoot, "src/components/ui")).sort()).toEqual(
    [
      "button",
      "checkbox",
      "context-menu",
      "dialog",
      "input",
      "label",
      "popover",
      "select",
      "sheet",
      "sonner",
      "tabs",
      "tooltip",
    ]
      .map((name) => `${name}.tsx`)
      .sort(),
  );

  const result = spawnSync(
    process.execPath,
    ["--input-type=module", "-e", buildAudit],
    {
      cwd: webRoot,
      encoding: "utf8",
      timeout: 60_000,
      maxBuffer: 4 * 1024 * 1024,
    },
  );
  expect(result.error).toBeUndefined();
  expect(result.status, result.stderr).toBe(0);
  const lines = result.stdout.trim().split("\n");
  const built: string = JSON.parse(lines.pop()!).css;
  console.log(lines.join("\n"));
  // Verify emitted declarations, not just source tokens or unused variable names.
  const expandHex = (value: string) =>
    value.length === 4
      ? "#" + [...value.slice(1)].map((digit) => digit + digit).join("")
      : value;
  for (const [index, selector] of [":root", ".dark"].entries()) {
    const declarations = built
      .slice(built.indexOf(`${selector}{--bg:`))
      .split("}")[0];
    for (const [name, values] of Object.entries(tokens)) {
      const value = declarations.match(
        new RegExp(`--${name}:(#[a-f0-9]+)(?:;|$)`),
      )?.[1];
      expect(value, `${selector} --${name}`).toBeDefined();
      expect(expandHex(value!)).toBe(values[index]);
    }
  }
  const builtBridge = built.match(/:root,\.dark\{([^}]+)\}/)![1];
  for (const [name, token] of Object.entries(aliases))
    expect(builtBridge).toContain(`--${name}:var(--${token})`);
  for (const utility of [
    ".bg-primary{background-color:var(--primary)}",
    ".focus\\:bg-accent:focus{background-color:var(--accent)}",
    ".text-foreground,.text-foreground\\/60{color:var(--foreground)}",
    ".border-border{border-color:var(--border)}",
    ".focus-visible\\:border-ring:focus-visible{border-color:var(--ring)}",
    // A class name glued to a template-literal `${` is invisible to the
    // scanner; this rule proves the active sidebar link colour is emitted.
    ".aria-\\[current\\=page\\]\\:text-accent-foreground[aria-current=page]{color:var(--accent-foreground)}",
  ])
    expect(built).toContain(utility);
  for (const state of ["open", "closed"]) {
    expect(built).toContain(`data-state=${state}`);
    expect(built).toContain(
      `.data-${state}\\:animate-${state === "open" ? "in" : "out"}`,
    );
  }
  expect(built).not.toMatch(/@font-face/);
  auditCSSURLs(built);
  console.log("BUILT_CSS_TOKEN_BRIDGE_AND_STATE_UTILITIES_OK");
}, 90_000);
