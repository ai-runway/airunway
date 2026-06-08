// ESLint flat config for the frontend (React + TypeScript).
//
// ESLint 9+ no longer reads `.eslintrc.*`; this flat config is the replacement.
// This file is loaded as ESM because the package sets `"type": "module"`.
import tseslint from '@typescript-eslint/eslint-plugin'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefreshMod from 'eslint-plugin-react-refresh'

// eslint-plugin-react-refresh ships as a CommonJS module that exposes its
// plugin object via an interop `default`. Node's ESM loader does not honor the
// `__esModule` marker, so unwrap it ourselves to get the real plugin (with
// `.rules`) regardless of how the interop resolves.
const reactRefresh = reactRefreshMod.default ?? reactRefreshMod

export default [
  // Build artifacts and generated output are never linted.
  { ignores: ['dist/**', 'build/**', 'coverage/**'] },

  // typescript-eslint's recommended flat config wires up the TS parser, the
  // `@typescript-eslint` plugin, and a sensible rule set. It also turns off
  // core rules (e.g. `no-undef`) that TypeScript already enforces, so we don't
  // need the `globals` package here.
  ...tseslint.configs['flat/recommended'],

  {
    files: ['**/*.{ts,tsx}'],
    plugins: {
      'react-hooks': reactHooks,
      'react-refresh': reactRefresh,
    },
    rules: {
      // The two battle-tested Hooks rules. (react-hooks v7 also ships
      // experimental react-compiler rules; we intentionally opt into only the
      // stable pair so the existing code base lints cleanly.)
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'warn',
      // Keep Fast Refresh working by flagging modules that export non-components
      // alongside components. Constant exports are allowed.
      'react-refresh/only-export-components': [
        'warn',
        { allowConstantExport: true },
      ],
    },
  },
]
