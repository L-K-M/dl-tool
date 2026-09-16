import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["dist"] },
  {
    files: ["src/**/*.{ts,tsx}"],
    extends: [tseslint.configs.recommended],
  },
  {
    files: ["src/**/*.tsx"],
    rules: {
      "no-restricted-syntax": [
        "error",
        {
          selector: "JSXText[value=/[A-Za-z]{2,}/]",
          message:
            "User-visible text must go through t(); add the key to a locale catalogue.",
        },
        {
          selector:
            ":matches(JSXElement, JSXFragment) > JSXExpressionContainer > Literal[value=/[A-Za-z]{2,}/]",
          message:
            "User-visible text must go through t(); add the key to a locale catalogue.",
        },
        {
          selector:
            "JSXAttribute[name.name=/^(title|placeholder|aria-label|alt)$/] > Literal",
          message: "User-visible attributes must go through t().",
        },
        {
          selector:
            "JSXAttribute[name.name=/^(title|placeholder|aria-label|alt)$/] > JSXExpressionContainer > :matches(Literal, TemplateLiteral[expressions.length=0])",
          message: "User-visible attributes must go through t().",
        },
      ],
    },
  },
);
