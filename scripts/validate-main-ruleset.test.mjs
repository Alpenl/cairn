import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

import { validateRuleset } from './validate-main-ruleset.mjs'

const policy = JSON.parse(await readFile(new URL('../.github/rulesets/main-policy.json', import.meta.url), 'utf8'))

function protectedRuleset() {
  return {
    name: 'main gate',
    target: 'branch',
    enforcement: 'active',
    conditions: { ref_name: { include: ['~DEFAULT_BRANCH'], exclude: [] } },
    bypass_actors: [],
    rules: [
      { type: 'deletion' },
      { type: 'non_fast_forward' },
      { type: 'required_linear_history' },
      { type: 'pull_request', parameters: { allowed_merge_methods: ['rebase', 'squash'] } },
      { type: 'required_status_checks', parameters: { required_status_checks: [{ context: 'gate' }, { context: 'lint-pr-commits' }] } },
    ],
  }
}

test('accepts the minimal main protection contract', () => {
  assert.deepEqual(validateRuleset(protectedRuleset(), policy), [])
})

test('rejects broad always bypass and missing protections', () => {
  const ruleset = protectedRuleset()
  ruleset.bypass_actors.push({ actor_type: 'RepositoryRole', actor_id: 5, bypass_mode: 'always' })
  ruleset.rules = ruleset.rules.filter((rule) => !['non_fast_forward', 'required_linear_history'].includes(rule.type))
  ruleset.rules.find((rule) => rule.type === 'required_status_checks').parameters.required_status_checks = [{ context: 'gate' }]

  const errors = validateRuleset(ruleset, policy)
  assert.ok(errors.some((error) => error.includes('RepositoryRole/always')))
  assert.ok(errors.some((error) => error.includes('non_fast_forward')))
  assert.ok(errors.some((error) => error.includes('required_linear_history')))
  assert.ok(errors.some((error) => error.includes('lint-pr-commits')))
})

test('CLI reads a ruleset snapshot from stdin', () => {
  const script = fileURLToPath(new URL('./validate-main-ruleset.mjs', import.meta.url))
  const policyPath = fileURLToPath(new URL('../.github/rulesets/main-policy.json', import.meta.url))
  const result = spawnSync(process.execPath, [script, '--policy', policyPath], {
    encoding: 'utf8',
    input: JSON.stringify(protectedRuleset()),
  })
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout, /matches the local protection policy/)
})
