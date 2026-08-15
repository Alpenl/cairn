import assert from 'node:assert/strict'
import test from 'node:test'

import { diagnoseJobs } from './ci-run-diagnose.mjs'

test('accepts a gate that really ran and succeeded', () => {
  const result = diagnoseJobs([
    { id: 1, name: 'changes', status: 'completed', conclusion: 'success', runner_id: 10, steps: [{ conclusion: 'success' }] },
    { id: 2, name: 'gate', status: 'completed', conclusion: 'success', runner_id: 11, steps: [{ conclusion: 'success' }] },
  ])
  assert.deepEqual(result, { errors: [], advice: [] })
})

test('rejects a skipped gate even when other jobs are green', () => {
  const result = diagnoseJobs([
    { id: 1, name: 'changes', status: 'completed', conclusion: 'success', runner_id: 10, steps: [{ conclusion: 'success' }] },
    { id: 2, name: 'gate', status: 'completed', conclusion: 'skipped', runner_id: 0, steps: [] },
  ])
  assert.ok(result.errors.some((error) => error.includes('was skipped')))
  assert.ok(result.errors.some((error) => error.includes('executed no steps')))
})

test('reports an actionable billing boundary without mutating it', () => {
  const result = diagnoseJobs(
    [
      { id: 94169224503, name: 'changes', status: 'completed', conclusion: 'failure', runner_id: 0, steps: [] },
      { id: 94169240665, name: 'gate', status: 'completed', conclusion: 'failure', runner_id: 0, steps: [] },
    ],
    {
      94169224503: [{ message: 'The job was not started because recent account payments have failed or your spending limit needs to be increased. Please check the Billing & plans section.' }],
    },
  )
  assert.ok(result.errors.some((error) => error.includes('before a runner')))
  assert.ok(result.advice.some((line) => line.includes('Billing & licensing')))
  assert.ok(result.advice.some((line) => line.includes('gh run rerun')))
})
