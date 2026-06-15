// ESLint flat config for the backend (Bun + Hono + TypeScript).
//
// ESLint 9+ no longer reads `.eslintrc.*`; this flat config is the replacement.
// The package is CommonJS (no `"type": "module"`), so this file uses the `.mjs`
// extension to be loaded as ESM.
import tseslint from '@typescript-eslint/eslint-plugin'

export default [
  // Build artifacts, compiled binaries, and generated output are never linted.
  { ignores: ['dist/**', 'build/**', 'coverage/**'] },

  // typescript-eslint's recommended flat config wires up the TS parser, the
  // `@typescript-eslint` plugin, and a sensible rule set. It also turns off
  // core rules (e.g. `no-undef`) that TypeScript already enforces, so we don't
  // need the `globals` package for Node/Bun globals like `process` or `Bun`.
  ...tseslint.configs['flat/recommended'],

  {
    files: ['**/*.ts'],
    rules: {
      // Allow intentionally-unused identifiers when prefixed with `_`
      // (e.g. unused function params kept for signature/positional reasons).
      '@typescript-eslint/no-unused-vars': [
        'error',
        {
          argsIgnorePattern: '^_',
          varsIgnorePattern: '^_',
          caughtErrorsIgnorePattern: '^_',
        },
      ],
    },
  },
]
