import { defineSharedApiContractTests } from '@webtag/api/testing'
import { describe, expect, it } from 'vitest'

defineSharedApiContractTests({
  group: (name, run) => describe(name, run),
  test: (name, run) => it(name, run),
  equal: (actual, expected) => expect(actual).toBe(expected),
})
