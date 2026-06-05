// Flat ESLint config (ESLint v9+). Replaces the old .eslintrc + `--ext` flow,
// which ESLint v9 removed. TypeScript is linted via the typescript-eslint
// plugin's `flat/recommended` preset, which bundles the parser, the plugin, and
// a non-type-checked rule set (fast, low false-positive noise).
import tseslint from '@typescript-eslint/eslint-plugin';
import tsparser from '@typescript-eslint/parser';

export default [
  // Only application source is linted (mirrors the previous `eslint src` scope).
  {
    ignores: ['dist/**', 'node_modules/**', '*.config.*', 'coverage/**'],
  },

  // typescript-eslint's flat/recommended turns off core rules that clash with
  // TS (e.g. no-undef), wires up the parser, and enables the recommended rules.
  ...tseslint.configs['flat/recommended'],

  {
    files: ['src/**/*.ts'],
    languageOptions: {
      parser: tsparser,
      ecmaVersion: 'latest',
      sourceType: 'module',
    },
    rules: {
      // Pre-existing debt: this codebase predates any ESLint config (the v8→v10
      // bump first introduced one), so `recommended` surfaces ~120 historical
      // violations of these two rules — none in newly-written code. Demote them
      // to warnings so lint is green and CI-usable today, while still surfacing
      // the backlog for incremental burndown. Promote back to "error" once the
      // existing hits are cleared.
      '@typescript-eslint/no-explicit-any': 'warn',
      '@typescript-eslint/no-unused-vars': 'warn',
    },
  },
];
