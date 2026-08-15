#!/usr/bin/env node

import { readFile } from 'node:fs/promises'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

const billingPattern = /payments? (?:have )?failed|spending limit|billing\s*&\s*plans/i

function jobAnnotations(job, annotationsByJob) {
  return annotationsByJob?.[String(job.id)] ?? []
}

export function diagnoseJobs(jobs, annotationsByJob = {}) {
  const errors = []
  const advice = []
  const gate = jobs.find((job) => job.name === 'gate')
  if (!gate) {
    errors.push('required gate job is missing from the workflow run')
  } else {
    if (gate.conclusion === 'skipped') errors.push('required gate job was skipped; skipped is not a successful gate signal')
    if (gate.status !== 'completed') errors.push(`required gate job status is ${JSON.stringify(gate.status)}, want "completed"`)
    if (gate.conclusion !== 'success') errors.push(`required gate job conclusion is ${JSON.stringify(gate.conclusion)}, want "success"`)
    if (!Array.isArray(gate.steps) || gate.steps.length === 0) errors.push('required gate job executed no steps')
  }

  let billingBlocked = false
  for (const job of jobs) {
    const annotations = jobAnnotations(job, annotationsByJob)
    const text = annotations.map((annotation) => annotation.message ?? '').join('\n')
    const noRunner = !job.runner_id && (!Array.isArray(job.steps) || job.steps.length === 0)
    if (noRunner && job.conclusion === 'failure') {
      errors.push(`${job.name} failed before a runner or any workflow step was allocated`)
    }
    if (billingPattern.test(text)) billingBlocked = true
  }
  if (billingBlocked) {
    advice.push('GitHub Actions was blocked at the account billing boundary. A repository owner must open Settings > Billing & licensing, resolve failed payment or the Actions spending limit, then rerun the failed workflow.')
    advice.push('After the account is usable, rerun with: gh run rerun RUN_ID --repo OWNER/REPO --failed')
    advice.push('Confirm that the final gate job is completed/success and has real steps; a skipped gate is not accepted.')
  }
  return { errors: [...new Set(errors)], advice }
}

function ghJSON(args) {
  const result = spawnSync('gh', ['api', ...args], { encoding: 'utf8' })
  if (result.error) throw result.error
  if (result.status !== 0) throw new Error(result.stderr.trim() || `gh api exited with ${result.status}`)
  return JSON.parse(result.stdout)
}

function loadRemote(repo, runID) {
  const pages = ghJSON(['--paginate', '--slurp', `/repos/${repo}/actions/runs/${runID}/jobs?filter=all&per_page=100`])
  const jobs = pages.flatMap((page) => page.jobs ?? [])
  const annotationsByJob = {}
  for (const job of jobs) {
    if (!job.id || (job.runner_id && job.steps?.length)) continue
    annotationsByJob[String(job.id)] = ghJSON([`/repos/${repo}/check-runs/${job.id}/annotations`])
  }
  return { jobs, annotations_by_job: annotationsByJob }
}

function parseArgs(args) {
  const options = {}
  for (let index = 0; index < args.length; index += 1) {
    const flag = args[index]
    if (flag === '--help') return { help: true }
    if (!['--input', '--repo', '--run-id'].includes(flag)) throw new Error(`unknown argument: ${flag}`)
    const value = args[index + 1]
    if (!value) throw new Error(`${flag} requires a value`)
    options[flag.slice(2).replace('-', '_')] = value
    index += 1
  }
  if (options.input && (options.repo || options.run_id)) throw new Error('--input cannot be combined with --repo or --run-id')
  if (!options.input && !(options.repo && options.run_id)) throw new Error('use --input PATH, or provide both --repo OWNER/REPO and --run-id ID')
  return options
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
    console.log('Usage: node scripts/ci-run-diagnose.mjs --repo OWNER/REPO --run-id ID')
    console.log('       node scripts/ci-run-diagnose.mjs --input jobs-and-annotations.json')
    console.log('Live mode performs read-only gh api calls. It never changes billing, reruns jobs, or updates repository settings.')
    return
  }

  try {
    const payload = options.input
      ? JSON.parse(await readFile(options.input, 'utf8'))
      : loadRemote(options.repo, options.run_id)
    const jobs = Array.isArray(payload) ? payload : payload.jobs ?? []
    const result = diagnoseJobs(jobs, payload.annotations_by_job ?? {})
    for (const job of jobs) console.log(`${job.name}: status=${job.status ?? 'unknown'} conclusion=${job.conclusion ?? 'unknown'} steps=${job.steps?.length ?? 0} runner_id=${job.runner_id ?? 0}`)
    for (const error of result.errors) console.error(`ERROR: ${error}`)
    for (const advice of result.advice) console.error(`ACTION: ${advice}`)
    if (result.errors.length > 0) process.exitCode = 1
  } catch (error) {
    console.error(`ERROR: ${error.message}`)
    process.exitCode = 2
  }
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) await main()
