#!/usr/bin/env node

import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'

function sameMembers(actual, expected) {
  const left = [...new Set(actual)].sort()
  const right = [...new Set(expected)].sort()
  return left.length === right.length && left.every((value, index) => value === right[index])
}

export function validateRuleset(ruleset, policy) {
  const errors = []
  if (ruleset.name !== policy.name) errors.push(`name is ${JSON.stringify(ruleset.name)}, want ${JSON.stringify(policy.name)}`)
  if (ruleset.target !== policy.target) errors.push(`target is ${JSON.stringify(ruleset.target)}, want ${JSON.stringify(policy.target)}`)
  if (ruleset.enforcement !== policy.enforcement) errors.push(`enforcement is ${JSON.stringify(ruleset.enforcement)}, want ${JSON.stringify(policy.enforcement)}`)

  const includes = ruleset.conditions?.ref_name?.include ?? []
  for (const required of policy.include ?? []) {
    if (!includes.includes(required)) errors.push(`ref_name.include is missing ${JSON.stringify(required)}`)
  }

  const rules = new Map((ruleset.rules ?? []).map((rule) => [rule.type, rule]))
  for (const required of policy.required_rules ?? []) {
    if (!rules.has(required)) errors.push(`required rule ${JSON.stringify(required)} is missing`)
  }

  const pullRequest = rules.get('pull_request')
  if (pullRequest) {
    const actual = pullRequest.parameters?.allowed_merge_methods ?? []
    if (!sameMembers(actual, policy.allowed_merge_methods ?? [])) {
      errors.push(`pull_request allowed_merge_methods are ${JSON.stringify(actual)}, want ${JSON.stringify(policy.allowed_merge_methods)}`)
    }
  }

  const statusRule = rules.get('required_status_checks')
  if (statusRule) {
    const actual = (statusRule.parameters?.required_status_checks ?? []).map((check) => check.context)
    for (const required of policy.required_status_checks ?? []) {
      if (!actual.includes(required)) errors.push(`required status check ${JSON.stringify(required)} is missing`)
    }
  }

  for (const bypass of ruleset.bypass_actors ?? []) {
    for (const forbidden of policy.forbidden_bypasses ?? []) {
      if (bypass.actor_type === forbidden.actor_type && bypass.bypass_mode === forbidden.bypass_mode) {
        errors.push(`forbidden bypass ${forbidden.actor_type}/${forbidden.bypass_mode} is configured`)
      }
    }
  }
  return errors
}

function parseArgs(args) {
  const options = { policy: '.github/rulesets/main-policy.json', input: '-' }
  for (let index = 0; index < args.length; index += 1) {
    const flag = args[index]
    if (flag === '--help') return { help: true }
    if (flag !== '--policy' && flag !== '--input') throw new Error(`unknown argument: ${flag}`)
    const value = args[index + 1]
    if (!value) throw new Error(`${flag} requires a value`)
    options[flag.slice(2)] = value
    index += 1
  }
  return options
}

async function readJSON(path) {
  let raw
  if (path === '-') {
    process.stdin.setEncoding('utf8')
    raw = ''
    for await (const chunk of process.stdin) raw += chunk
  } else {
    raw = await readFile(path, 'utf8')
  }
  return JSON.parse(raw)
}

async function main() {
  let options
  try {
    options = parseArgs(process.argv.slice(2))
  } catch (error) {
    console.error(error.message)
    process.exitCode = 2
    return
  }
  if (options.help) {
    console.log('Usage: gh api repos/OWNER/REPO/rulesets/ID | node scripts/validate-main-ruleset.mjs [--policy PATH] [--input PATH|-]')
    console.log('The validator is read-only: it consumes a ruleset JSON snapshot and never calls a remote API.')
    return
  }

  try {
    const [ruleset, policy] = await Promise.all([readJSON(options.input), readJSON(options.policy)])
    const errors = validateRuleset(ruleset, policy)
    if (errors.length === 0) {
      console.log('main ruleset matches the local protection policy')
      return
    }
    for (const error of errors) console.error(`ERROR: ${error}`)
    process.exitCode = 1
  } catch (error) {
    console.error(`ERROR: ${error.message}`)
    process.exitCode = 2
  }
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) await main()
