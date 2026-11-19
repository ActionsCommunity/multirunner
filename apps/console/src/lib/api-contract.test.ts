import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, test } from 'vitest'

interface ContractSchema {
  $defs: {
    error: { required: string[] }
    collection: { required: string[] }
  }
  'x-resource-groups': string[]
  'x-routes': { method: string; path: string }[]
}

describe('console API contract artifact', () => {
  const schema = JSON.parse(
    readFileSync(
      resolve(
        process.cwd(),
        '../../internal/consoleapi/api-contract.schema.json',
      ),
      'utf8',
    ),
  ) as ContractSchema

  test('defines stable collection and error envelopes', () => {
    expect(schema.$defs.collection.required).toEqual([
      'items',
      'applied_filters',
      'cursor',
      'next_cursor',
      'count',
    ])
    expect(schema.$defs.error.required).toEqual([
      'code',
      'message',
      'correlation_id',
      'field_violations',
      'retry',
    ])
  })

  test('covers every required v1 resource group', () => {
    expect(schema['x-resource-groups']).toHaveLength(26)
    expect(schema['x-resource-groups']).toContain('audit')
    expect(schema['x-resource-groups']).toContain('tags')
    expect(schema['x-resource-groups']).toContain('restores')
    expect(schema['x-resource-groups']).toContain('exports')
  })

  test('covers registered non-collection routes', () => {
    const routes = schema['x-routes'].map(
      ({ method, path }) => `${method} ${path}`,
    )
    expect(routes).toContain('GET /api/v1/session')
    expect(routes).toContain('GET /api/v1/support-bundles/{id}')
    expect(routes).toContain('GET /api/v1/jobs/{id}/log')
    expect(routes).toContain('GET /api/v1/restores')
    expect(routes).toContain('POST /api/v1/exports')
    expect(routes).toContain('GET /api/v1/exports/{id}')
  })
})
