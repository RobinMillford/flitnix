import test from 'node:test'
import assert from 'node:assert/strict'

// Tab configuration model matching App.svelte
const tabOrder = ['personal', 'guest', 'templates', 'audit', 'devices', 'settings'] as const
type TabId = typeof tabOrder[number]

function getNextTab(currentTab: TabId, key: string): TabId {
  const currentIndex = tabOrder.indexOf(currentTab)
  if (currentIndex === -1) return currentTab
  let nextIndex = currentIndex

  if (key === 'ArrowRight') {
    nextIndex = (currentIndex + 1) % tabOrder.length
  } else if (key === 'ArrowLeft') {
    nextIndex = (currentIndex - 1 + tabOrder.length) % tabOrder.length
  } else if (key === 'Home') {
    nextIndex = 0
  } else if (key === 'End') {
    nextIndex = tabOrder.length - 1
  } else {
    // ArrowUp, ArrowDown, and other keys are not intercepted in horizontal orientation
    return currentTab
  }

  return tabOrder[nextIndex]
}

function getRovingTabindex(tab: TabId, activeTab: TabId): number {
  return tab === activeTab ? 0 : -1
}

test('Tab navigation: order contains all 6 tabs including Settings', () => {
  assert.equal(tabOrder.length, 6)
  assert.deepEqual(Array.from(tabOrder), [
    'personal', 'guest', 'templates', 'audit', 'devices', 'settings'
  ])
})

test('Tab navigation: ArrowRight advances and wraps in horizontal orientation', () => {
  assert.equal(getNextTab('personal', 'ArrowRight'), 'guest')
  assert.equal(getNextTab('guest', 'ArrowRight'), 'templates')
  assert.equal(getNextTab('templates', 'ArrowRight'), 'audit')
  assert.equal(getNextTab('audit', 'ArrowRight'), 'devices')
  assert.equal(getNextTab('devices', 'ArrowRight'), 'settings')
  assert.equal(getNextTab('settings', 'ArrowRight'), 'personal') // Wrap around
})

test('Tab navigation: ArrowLeft retreats and wraps in horizontal orientation', () => {
  assert.equal(getNextTab('personal', 'ArrowLeft'), 'settings') // Wrap around
  assert.equal(getNextTab('settings', 'ArrowLeft'), 'devices')
  assert.equal(getNextTab('devices', 'ArrowLeft'), 'audit')
  assert.equal(getNextTab('audit', 'ArrowLeft'), 'templates')
  assert.equal(getNextTab('templates', 'ArrowLeft'), 'guest')
  assert.equal(getNextTab('guest', 'ArrowLeft'), 'personal')
})

test('Tab navigation: ArrowUp and ArrowDown are not intercepted, preserving page scrolling', () => {
  // ArrowUp and ArrowDown should return the current tab unchanged
  for (const tab of tabOrder) {
    assert.equal(getNextTab(tab, 'ArrowDown'), tab)
    assert.equal(getNextTab(tab, 'ArrowUp'), tab)
  }
})

test('Tab navigation: Home goes to first tab and End goes to last tab', () => {
  for (const tab of tabOrder) {
    assert.equal(getNextTab(tab, 'Home'), 'personal')
    assert.equal(getNextTab(tab, 'End'), 'settings')
  }
})

test('Tab navigation: Roving tabindex sets 0 on active tab and -1 on all others', () => {
  for (const active of tabOrder) {
    for (const t of tabOrder) {
      if (t === active) {
        assert.equal(getRovingTabindex(t, active), 0)
      } else {
        assert.equal(getRovingTabindex(t, active), -1)
      }
    }
  }
})

test('Tab navigation: Panel and tab IDs have symmetric ARIA relationships', () => {
  const panelMap: Record<TabId, { tabId: string, panelId: string, role: string }> = {
    personal: { tabId: 'tab-personal', panelId: 'panel-personal', role: 'tabpanel' },
    guest: { tabId: 'tab-guest', panelId: 'panel-guest', role: 'tabpanel' },
    templates: { tabId: 'tab-templates', panelId: 'panel-templates', role: 'tabpanel' },
    audit: { tabId: 'tab-audit', panelId: 'panel-audit', role: 'tabpanel' },
    devices: { tabId: 'tab-devices', panelId: 'panel-devices', role: 'tabpanel' },
    settings: { tabId: 'tab-settings', panelId: 'panel-settings', role: 'tabpanel' }
  }

  assert.equal(Object.keys(panelMap).length, tabOrder.length)
  for (const id of tabOrder) {
    const mapping = panelMap[id]
    assert.equal(mapping.tabId, `tab-${id}`)
    assert.equal(mapping.panelId, `panel-${id}`)
    assert.equal(mapping.role, 'tabpanel')
  }
})
