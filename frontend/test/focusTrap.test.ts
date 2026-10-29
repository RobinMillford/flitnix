import test from 'node:test'
import assert from 'node:assert/strict'
import { focusTrap } from '../src/focusTrap.ts'

class MockElement {
  tagName: string
  attributes: Record<string, string> = {}
  children: MockElement[] = []
  parentNode: MockElement | null = null
  focused = false
  disabled = false
  hidden = false

  constructor(tagName: string) {
    this.tagName = tagName.toUpperCase()
  }

  setAttribute(k: string, v: string) {
    this.attributes[k] = v
  }

  getAttribute(k: string): string | null {
    return this.attributes[k] ?? null
  }

  hasAttribute(k: string): boolean {
    return k in this.attributes
  }

  focus() {
    this.focused = true
    if (globalThis.document) {
      (globalThis.document as any).activeElement = this
    }
  }

  contains(el: any): boolean {
    if (el === this) return true
    for (const child of this.children) {
      if (child.contains(el)) return true
    }
    return false
  }

  querySelectorAll(selector: string): MockElement[] {
    const results: MockElement[] = []
    const walk = (node: MockElement) => {
      for (const child of node.children) {
        if (child.tagName === 'BUTTON' || child.tagName === 'INPUT' || child.tagName === 'A') {
          results.push(child)
        }
        walk(child)
      }
    }
    walk(this)
    return results
  }
}

test('focusTrap sets initial focus to first element', () => {
  const listeners: Record<string, (e: any) => void> = {}
  const mockWindow = {
    addEventListener: (type: string, fn: any) => { listeners[type] = fn },
    removeEventListener: (type: string) => { delete listeners[type] }
  }
  const prevWindow = (globalThis as any).window;
  const prevDoc = (globalThis as any).document;
  (globalThis as any).window = mockWindow;
  const mockDoc = { activeElement: null };
  (globalThis as any).document = mockDoc;

  try {
    const modal = new MockElement('div')
    const btn1 = new MockElement('button')
    const btn2 = new MockElement('button')
    modal.children.push(btn1, btn2)

    const trap = focusTrap(modal as any)
    assert.equal(btn1.focused, true)
    assert.equal(mockDoc.activeElement, btn1)
    trap.destroy()
  } finally {
    (globalThis as any).window = prevWindow
    ;(globalThis as any).document = prevDoc
  }
})

test('focusTrap invokes onEscape when Escape key is pressed', () => {
  const listeners: Record<string, (e: any) => void> = {}
  const mockWindow = {
    addEventListener: (type: string, fn: any) => { listeners[type] = fn },
    removeEventListener: (type: string) => { delete listeners[type] }
  }
  const prevWindow = (globalThis as any).window;
  const prevDoc = (globalThis as any).document;
  (globalThis as any).window = mockWindow;
  (globalThis as any).document = { activeElement: null };

  try {
    const modal = new MockElement('div')
    let escaped = false
    const trap = focusTrap(modal as any, {
      onEscape: () => { escaped = true }
    })

    let defaultPrevented = false
    let propagationStopped = false
    const event = {
      key: 'Escape',
      preventDefault: () => { defaultPrevented = true },
      stopPropagation: () => { propagationStopped = true }
    }
    listeners['keydown'](event)
    assert.equal(escaped, true)
    assert.equal(defaultPrevented, true)
    assert.equal(propagationStopped, true)
    trap.destroy()
  } finally {
    (globalThis as any).window = prevWindow
    ;(globalThis as any).document = prevDoc
  }
})

test('focusTrap wraps Tab from last to first element', () => {
  const listeners: Record<string, (e: any) => void> = {}
  const mockWindow = {
    addEventListener: (type: string, fn: any) => { listeners[type] = fn },
    removeEventListener: (type: string) => { delete listeners[type] }
  }
  const prevWindow = (globalThis as any).window;
  const prevDoc = (globalThis as any).document;
  (globalThis as any).window = mockWindow;

  const modal = new MockElement('div');
  const btn1 = new MockElement('button');
  const btn2 = new MockElement('button');
  modal.children.push(btn1, btn2);

  const mockDoc = { activeElement: btn2 };
  (globalThis as any).document = mockDoc;

  try {
    const trap = focusTrap(modal as any)
    btn1.focused = false
    btn2.focused = false
    mockDoc.activeElement = btn2

    let prevented = false
    listeners['keydown']({
      key: 'Tab',
      shiftKey: false,
      preventDefault: () => { prevented = true },
      stopPropagation: () => {}
    })

    assert.equal(prevented, true)
    assert.equal(btn1.focused, true)
    trap.destroy()
  } finally {
    (globalThis as any).window = prevWindow
    ;(globalThis as any).document = prevDoc
  }
})

test('focusTrap wraps Shift+Tab from first to last element', () => {
  const listeners: Record<string, (e: any) => void> = {}
  const mockWindow = {
    addEventListener: (type: string, fn: any) => { listeners[type] = fn },
    removeEventListener: (type: string) => { delete listeners[type] }
  }
  const prevWindow = (globalThis as any).window;
  const prevDoc = (globalThis as any).document;
  (globalThis as any).window = mockWindow;

  const modal = new MockElement('div');
  const btn1 = new MockElement('button');
  const btn2 = new MockElement('button');
  modal.children.push(btn1, btn2);

  const mockDoc = { activeElement: btn1 };
  (globalThis as any).document = mockDoc;

  try {
    const trap = focusTrap(modal as any)
    btn1.focused = false
    btn2.focused = false
    mockDoc.activeElement = btn1

    let prevented = false
    listeners['keydown']({
      key: 'Tab',
      shiftKey: true,
      preventDefault: () => { prevented = true },
      stopPropagation: () => {}
    })

    assert.equal(prevented, true)
    assert.equal(btn2.focused, true)
    trap.destroy()
  } finally {
    (globalThis as any).window = prevWindow
    ;(globalThis as any).document = prevDoc
  }
})

test('focusTrap handles dialog with 0 focusable children safely', () => {
  const listeners: Record<string, (e: any) => void> = {}
  const mockWindow = {
    addEventListener: (type: string, fn: any) => { listeners[type] = fn },
    removeEventListener: (type: string) => { delete listeners[type] }
  }
  const prevWindow = (globalThis as any).window;
  const prevDoc = (globalThis as any).document;
  (globalThis as any).window = mockWindow;
  const mockDoc = { activeElement: null };
  (globalThis as any).document = mockDoc;

  try {
    const emptyModal = new MockElement('div')
    const trap = focusTrap(emptyModal as any)

    assert.equal(emptyModal.focused, true)
    assert.equal(emptyModal.getAttribute('tabindex'), '-1')

    let prevented = false
    listeners['keydown']({
      key: 'Tab',
      shiftKey: false,
      preventDefault: () => { prevented = true },
      stopPropagation: () => {}
    })
    assert.equal(prevented, true)
    trap.destroy()
  } finally {
    (globalThis as any).window = prevWindow
    ;(globalThis as any).document = prevDoc
  }
})

test('focusTrap skips disabled or hidden elements', () => {
  const listeners: Record<string, (e: any) => void> = {}
  const mockWindow = {
    addEventListener: (type: string, fn: any) => { listeners[type] = fn },
    removeEventListener: (type: string) => { delete listeners[type] }
  }
  const prevWindow = (globalThis as any).window;
  const prevDoc = (globalThis as any).document;
  (globalThis as any).window = mockWindow;

  const modal = new MockElement('div');
  const disabledBtn = new MockElement('button');
  disabledBtn.disabled = true;
  const hiddenBtn = new MockElement('button');
  hiddenBtn.hidden = true;
  const activeBtn = new MockElement('button');
  modal.children.push(disabledBtn, hiddenBtn, activeBtn);

  const mockDoc = { activeElement: null };
  (globalThis as any).document = mockDoc;

  try {
    const trap = focusTrap(modal as any)
    assert.equal(activeBtn.focused, true)
    assert.equal(disabledBtn.focused, false)
    assert.equal(hiddenBtn.focused, false)
    trap.destroy()
  } finally {
    (globalThis as any).window = prevWindow
    ;(globalThis as any).document = prevDoc
  }
})

test('focusTrap restores focus to launcher button upon destroy', async () => {
  const listeners: Record<string, (e: any) => void> = {}
  const mockWindow = {
    addEventListener: (type: string, fn: any) => { listeners[type] = fn },
    removeEventListener: (type: string) => { delete listeners[type] }
  }
  const prevWindow = (globalThis as any).window;
  const prevDoc = (globalThis as any).document;
  (globalThis as any).window = mockWindow;

  const launcher = new MockElement('button');
  launcher.focused = true;
  const mockDoc = {
    activeElement: launcher,
    contains: (el: any) => el === launcher
  };
  (globalThis as any).document = mockDoc;

  try {
    const modal = new MockElement('div')
    const innerBtn = new MockElement('button')
    modal.children.push(innerBtn)

    const trap = focusTrap(modal as any)
    assert.equal(innerBtn.focused, true)
    launcher.focused = false

    trap.destroy()
    // Wait for microtask/setTimeout
    await new Promise((r) => setTimeout(r, 10))

    assert.equal(launcher.focused, true)
  } finally {
    (globalThis as any).window = prevWindow
    ;(globalThis as any).document = prevDoc
  }
})

test('focusTrap restores focus to different custom launchers', async () => {
  const listeners: Record<string, (e: any) => void> = {}
  const mockWindow = {
    addEventListener: (type: string, fn: any) => { listeners[type] = fn },
    removeEventListener: (type: string) => { delete listeners[type] }
  }
  const prevWindow = (globalThis as any).window;
  const prevDoc = (globalThis as any).document;
  (globalThis as any).window = mockWindow;

  const launcherA = new MockElement('button');
  const launcherB = new MockElement('button');
  const mockDoc = {
    activeElement: launcherB,
    contains: (_el: any) => true
  };
  (globalThis as any).document = mockDoc;

  try {
    const modal = new MockElement('div')
    const innerBtn = new MockElement('button')
    modal.children.push(innerBtn)

    // Explicit returnFocus option tested
    const trap = focusTrap(modal as any, { returnFocus: launcherA as any })
    assert.equal(innerBtn.focused, true)

    trap.destroy()
    await new Promise((r) => setTimeout(r, 10))

    assert.equal(launcherA.focused, true)
    assert.equal(launcherB.focused, false)
  } finally {
    (globalThis as any).window = prevWindow
    ;(globalThis as any).document = prevDoc
  }
})

test('focusTrap does not restore focus if trigger was disconnected or disabled', async () => {
  const listeners: Record<string, (e: any) => void> = {}
  const mockWindow = {
    addEventListener: (type: string, fn: any) => { listeners[type] = fn },
    removeEventListener: (type: string) => { delete listeners[type] }
  }
  const prevWindow = (globalThis as any).window;
  const prevDoc = (globalThis as any).document;
  (globalThis as any).window = mockWindow;

  const launcher = new MockElement('button');
  let inDom = true;
  const mockDoc = {
    activeElement: launcher,
    contains: (_el: any) => inDom
  };
  (globalThis as any).document = mockDoc;

  try {
    const modal = new MockElement('div')
    const innerBtn = new MockElement('button')
    modal.children.push(innerBtn)

    const trap = focusTrap(modal as any)
    inDom = false // Trigger removed from DOM while dialog was open

    trap.destroy()
    launcher.focused = false
    await new Promise((r) => setTimeout(r, 10))

    assert.equal(launcher.focused, false)

    // Now test disabled trigger
    inDom = true
    launcher.disabled = true
    const trap2 = focusTrap(modal as any, { returnFocus: launcher as any })
    trap2.destroy()
    launcher.focused = false
    await new Promise((r) => setTimeout(r, 10))

    assert.equal(launcher.focused, false)
  } finally {
    (globalThis as any).window = prevWindow
    ;(globalThis as any).document = prevDoc
  }
})
