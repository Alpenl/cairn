export type SourceBlockKind = 'summary'

export interface SourceBlockId {
  readonly namespace: string
  readonly linkId: string
  readonly blockKind: SourceBlockKind
  readonly sourceHash: string
}

export function isValidSourceIdentityComponent(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0 && !value.includes('\0')
}

export function isValidLinkId(value: unknown): value is string {
  return isValidSourceIdentityComponent(value)
}

export function isValidSourceHash(value: unknown): value is string {
  return typeof value === 'string' && /^[0-9a-f]{64}$/.test(value)
}

export function sourceBlockKey(id: SourceBlockId): string {
  if (
    !isValidSourceIdentityComponent(id.namespace) ||
    !isValidLinkId(id.linkId)
  ) {
    throw new Error('Source block identity is invalid')
  }
  if (id.blockKind !== 'summary') throw new Error('Unsupported source block kind')
  if (!isValidSourceHash(id.sourceHash)) {
    throw new Error('Source block hash is invalid')
  }
  return `${id.namespace}\0${id.linkId}\0${id.blockKind}\0${id.sourceHash}`
}
