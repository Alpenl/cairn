// Reader 前端 ESLint 配置（传统 .eslintrc，ESLint 8）。
// 参照 extension 的规则风格：TS strict、禁用 any、React Hooks 规则、react-refresh。
module.exports = {
  root: true,
  env: { browser: true, es2021: true, node: true },
  parser: '@typescript-eslint/parser',
  parserOptions: {
    ecmaVersion: 'latest',
    sourceType: 'module',
    ecmaFeatures: { jsx: true },
  },
  settings: { react: { version: '18.3' } },
  plugins: ['@typescript-eslint', 'react-hooks', 'react-refresh'],
  extends: [
    'eslint:recommended',
    'plugin:@typescript-eslint/recommended',
    'plugin:react-hooks/recommended',
  ],
  ignorePatterns: ['dist', 'types', 'node_modules', '*.cjs'],
  rules: {
    // TS 已做符号解析，no-undef 在 TS 项目里只会误报（全局类型由 tsconfig types 提供）。
    'no-undef': 'off',
    '@typescript-eslint/no-explicit-any': 'error',
    '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_' }],
    'react-refresh/only-export-components': ['warn', { allowConstantExport: true }],
  },
}
