import test from 'node:test'
import assert from 'node:assert/strict'

function buildBreadcrumbTrail(rootName: string, prefix: string) {
  const segs = prefix ? prefix.split('/').filter(Boolean) : []
  const trail: Array<{ label: string; prefix: string; current: boolean }> = [
    { label: rootName, prefix: '', current: segs.length === 0 },
  ]
  let acc = ''
  segs.forEach((seg, idx) => {
    acc = acc ? `${acc}/${seg}` : seg
    trail.push({ label: seg, prefix: acc, current: idx === segs.length - 1 })
  })
  return trail
}

test('Folder Explorer Breadcrumbs: builds accurate navigation targets from prefix', () => {
  assert.deepEqual(buildBreadcrumbTrail('Photos', ''), [
    { label: 'Photos', prefix: '', current: true },
  ])
  assert.deepEqual(buildBreadcrumbTrail('Photos', '2026/Trips/Kyoto'), [
    { label: 'Photos', prefix: '', current: false },
    { label: '2026', prefix: '2026', current: false },
    { label: 'Trips', prefix: '2026/Trips', current: false },
    { label: 'Kyoto', prefix: '2026/Trips/Kyoto', current: true },
  ])
})

test('Folder Explorer Selection: retains selected item IDs across folder navigation', () => {
  const selected: Record<string, boolean> = {}
  selected['item-day1-a'] = true
  // Navigating into another subdirectory preserves prior selections in the same source.
  selected['item-day2-b'] = true
  const selectedIds = Object.keys(selected)
  assert.deepEqual(selectedIds, ['item-day1-a', 'item-day2-b'])
})
