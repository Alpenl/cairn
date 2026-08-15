// Commitlint configuration for WebTag.
//
// We extend @commitlint/config-conventional and pin the type list to the
// vocabulary actually used in this repo's history (see `git log --oneline`).
// Conventional Commits keep `git-cliff` happy and let the commitlint
// GitHub workflow reject PR commits that drift from the contract.
//
// File name is .mjs (not .js): wagoid/commitlint-github-action v6+ refuses
// CommonJS .js configs ("`.js` extension is not allowed for the `configFile`").
//
// References:
//   https://www.conventionalcommits.org/en/v1.0.0/
//   https://github.com/conventional-changelog/commitlint
export default {
  extends: ['@commitlint/config-conventional'],
  rules: {
    'type-enum': [
      2,
      'always',
      [
        'feat',
        'fix',
        'docs',
        'style',
        'refactor',
        'perf',
        'test',
        'build',
        'ci',
        'chore',
        'revert',
      ],
    ],
    // Headers up to 100 chars — the default 72 is too tight for our scopes.
    'header-max-length': [2, 'always', 100],
    'subject-case': [0],
    'body-max-line-length': [0],
    'footer-max-line-length': [0],
  },
};
