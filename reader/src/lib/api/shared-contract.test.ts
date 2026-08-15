import { defineSharedApiContractTests } from '@webtag/api/testing'

defineSharedApiContractTests({
  group: (name, run) => describe(name, run),
  test: (name, run) => it(name, run),
  equal: (actual, expected) => expect(actual).toBe(expected),
})
