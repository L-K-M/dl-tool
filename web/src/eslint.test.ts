import { ESLint } from "eslint";
import { expect, test } from "vitest";

// The no-restricted-syntax selectors live in eslint.config.js; lint virtual
// fixtures through the real config so the rules stay honest.
const lint = async (code: string) => {
  const [result] = await new ESLint({ cwd: process.cwd() }).lintText(code, {
    filePath: "src/__fixture__.tsx",
  });
  return result.messages;
};

test("TestUntranslatedJsxTextRuleCoversTemplateChildren", async () => {
  // String-literal and template-literal expression children are both
  // user-visible text and must go through t().
  for (const child of ['{"Hello world"}', "{`Hello world`}"]) {
    const messages = await lint(`export const C = () => <p>${child}</p>;\n`);
    expect(
      messages.filter((m) => m.ruleId === "no-restricted-syntax"),
      child,
    ).toHaveLength(1);
  }
  // The JSX whitespace idiom, translated text and a <style> sheet — which is
  // not user-visible text — stay legal.
  for (const child of ["{` `}", '{t("general.theme")}']) {
    const messages = await lint(
      `declare const t: (k: string) => string;\nexport const C = () => <p>${child}</p>;\n`,
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
