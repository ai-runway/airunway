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

      // Enforced as errors: these were cleaned up across the codebase, so they
      // stay green and must not regress. `no-unused-vars` allows intentionally
      // unused identifiers when prefixed with `_` (e.g. positional params).
      '@typescript-eslint/no-explicit-any': 'error',
      '@typescript-eslint/no-empty-object-type': 'error',
      '@typescript-eslint/no-unused-vars': [
        'error',
        {
          argsIgnorePattern: '^_',
          varsIgnorePattern: '^_',
          caughtErrorsIgnorePattern: '^_',
        },
      ],

      // Pre-existing debt (NOT in scope for this config's cleanup): react-hooks
      // v7 enabled new experimental React-Compiler rules that surface historical
      // violations in older components. Demote to warnings so lint is green and
      // CI-usable today while still surfacing the backlog for incremental
      // burndown. Promote back to "error" once each is cleared.
      'react-hooks/set-state-in-effect': 'warn',
      'react-hooks/purity': 'warn',
      'react-hooks/preserve-manual-memoization': 'warn',
    },
  },
];
