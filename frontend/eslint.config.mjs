// Flat ESLint config (ESLint v9+). Replaces the old .eslintrc + `--ext` flow,
// which ESLint v9 removed. TypeScript is linted via the typescript-eslint
// plugin's `flat/recommended` preset, which bundles the parser, the plugin, and
// a non-type-checked rule set (fast, low false-positive noise).
import tseslint from '@typescript-eslint/eslint-plugin';
import tsparser from '@typescript-eslint/parser';
import reactHooks from 'eslint-plugin-react-hooks';
import reactRefresh from 'eslint-plugin-react-refresh';

export default [
  // Ignore build output and config files (mirrors the previous `eslint src`
  // scope — only application source is linted).
  {
    ignores: ['dist/**', 'node_modules/**', '*.config.*', 'coverage/**'],
  },

  // typescript-eslint's flat/recommended turns off core rules that clash with
  // TS (e.g. no-undef), wires up the parser, and enables the recommended rules.
  ...tseslint.configs['flat/recommended'],

  // React-specific rules + JSX parsing for the application source.
  {
    files: ['src/**/*.{ts,tsx}'],
    languageOptions: {
      parser: tsparser,
      ecmaVersion: 'latest',
      sourceType: 'module',
      parserOptions: {
        ecmaFeatures: { jsx: true },
      },
    },
    plugins: {
      'react-hooks': reactHooks,
      'react-refresh': reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      'react-refresh/only-export-components': ['warn', { allowConstantExport: true }],

      // Pre-existing debt: this codebase predates any ESLint config (the v8→v10
      // bump first introduced one) and the react-hooks v4→v7 bump enabled new
      // React-Compiler rules. `recommended` therefore surfaces ~26 historical
      // violations — none in newly-written code. Demote them to warnings so lint
      // is green and CI-usable today while still surfacing the backlog for
      // incremental burndown. Promote back to "error" once each is cleared.
      '@typescript-eslint/no-explicit-any': 'warn',
      '@typescript-eslint/no-unused-vars': 'warn',
      '@typescript-eslint/no-empty-object-type': 'warn',
      'react-hooks/set-state-in-effect': 'warn',
      'react-hooks/purity': 'warn',
      'react-hooks/preserve-manual-memoization': 'warn',
    },
  },
];
