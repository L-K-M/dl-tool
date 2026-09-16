import { ESLint } from "eslint";
import { expect, test } from "vitest";

// The no-restricted-syntax selectors live in eslint.config.js; lint virtual
// fixtures through the real config so the rules stay honest. One instance is
// shared across calls — an ESLint object is reusable, and constructing one
// per fixture would pay the flat-config load each time.
const eslint = new ESLint({ cwd: process.cwd() });

const lint = async (code: string) => {
  const [result] = await eslint.lintText(code, {
    filePath: "src/__fixture__.tsx",
  });
  return result.messages;
};

test("TestUntranslatedJsxTextRuleCoversTemplateChildren", async () => {
  // String-literal and template-literal expression children are both
  // user-visible text and must go through t(); interpolation does not
  // excuse a letter-bearing quasi.
  for (const child of [
    '{"Hello world"}',
    "{`Hello world`}",
    "{`Hello ${name}`}",
  ]) {
    const messages = await lint(
      `declare const name: string;\nexport const C = () => <p>${child}</p>;\n`,
    );
    expect(
      messages.filter((m) => m.ruleId === "no-restricted-syntax"),
      child,
    ).toHaveLength(1);
  }
  // The JSX whitespace idiom, translated text, a template whose quasis
  // carry no words and a <style> sheet — not user-visible text — stay
  // legal.
  for (const child of ["{` `}", "{`${a}-${b}`}", '{t("general.theme")}']) {
    const messages = await lint(
      `declare const t: (k: string) => string;\ndeclare const a: string;\ndeclare const b: string;\nexport const C = () => <p>${child}</p>;\n`,
    );
    expect(
      messages.filter((m) => m.ruleId === "no-restricted-syntax"),
      child,
    ).toHaveLength(0);
  }
  const stylesheet = await lint(
    "export const C = () => <style>{`.a { color: red; }`}</style>;\n",
  );
  expect(
    stylesheet.filter((m) => m.ruleId === "no-restricted-syntax"),
  ).toHaveLength(0);
});
