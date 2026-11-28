import { defineConfig } from "vite-plus";

export default defineConfig({
  fmt: {},
  lint: {
    plugins: ["typescript", "oxc"],
    options: {
      typeAware: true,
      typeCheck: true,
    },
    rules: {
      "typescript/no-unsafe-declaration-merging": "off",
    },
  },
  test: {
    environment: "node",
    include: ["test/**/*.test.ts"],
  },
});
