/**
 * focusTrap Svelte action
 *
 * Traps Tab and Shift+Tab navigation strictly within node,
 * handles Escape key closure, and restores focus to the trigger
 * element upon modal destruction provided it is still usable.
 */

export interface FocusTrapOptions {
  onEscape?: () => void
  returnFocus?: HTMLElement | null
  initialFocus?: HTMLElement | null
}

export function focusTrap(node: HTMLElement, options: FocusTrapOptions = {}) {
  const trigger = (options.returnFocus ?? (typeof document !== 'undefined' ? document.activeElement : null)) as HTMLElement | null

  const getFocusables = (): HTMLElement[] => {
    if (typeof node.querySelectorAll !== 'function') return []
    const selector = [
      'a[href]',
      'button:not([disabled]):not([aria-hidden="true"])',
      'input:not([disabled]):not([type="hidden"])',
      'select:not([disabled])',
      'textarea:not([disabled])',
      '[tabindex]:not([tabindex="-1"])'
    ].join(', ')

    return Array.from(node.querySelectorAll<HTMLElement>(selector)).filter((el) => {
      if ((el as any).disabled) return false
      if (el.getAttribute('aria-hidden') === 'true') return false
      if (el.getAttribute('tabindex') === '-1') return false
      if (el.hasAttribute('hidden') || (el as any).hidden === true) return false

      if (typeof window !== 'undefined' && typeof window.getComputedStyle === 'function') {
        const style = window.getComputedStyle(el)
        if (style.display === 'none' || style.visibility === 'hidden' || style.visibility === 'collapse') {
          return false
        }
      }

      if (typeof el.getClientRects === 'function') {
        const rects = el.getClientRects()
        if (rects && rects.length === 0 && el.offsetWidth === 0 && el.offsetHeight === 0) {
          if (typeof window !== 'undefined' && typeof window.getComputedStyle === 'function') {
            return false
          }
        }
      }

      return true
    })
  }

  // Initial focus
  const focusables = getFocusables()
  if (options.initialFocus && typeof options.initialFocus.focus === 'function') {
    options.initialFocus.focus()
  } else if (focusables.length > 0 && typeof focusables[0].focus === 'function') {
    focusables[0].focus()
  } else if (typeof node.focus === 'function') {
    if (!node.hasAttribute('tabindex')) {
      node.setAttribute('tabindex', '-1')
    }
    node.focus()
  }

  const handleKeydown = (e: KeyboardEvent) => {
    if (e.key === 'Escape') {
      e.stopPropagation()
      e.preventDefault()
      options.onEscape?.()
      return
    }

    if (e.key !== 'Tab') return

    const currentFocusables = getFocusables()
    if (currentFocusables.length === 0) {
      e.preventDefault()
      if (typeof node.focus === 'function' && typeof document !== 'undefined' && document.activeElement !== node) {
        node.focus()
      }
      return
    }

    if (currentFocusables.length === 1) {
      e.preventDefault()
      currentFocusables[0].focus()
      return
    }

    const first = currentFocusables[0]
    const last = currentFocusables[currentFocusables.length - 1]

    if (e.shiftKey) {
      if (document.activeElement === first || !node.contains(document.activeElement)) {
        last.focus()
        e.preventDefault()
      }
    } else {
      if (document.activeElement === last || !node.contains(document.activeElement)) {
        first.focus()
        e.preventDefault()
      }
    }
  }

  if (typeof window !== 'undefined') {
    window.addEventListener('keydown', handleKeydown, true)
  }

  return {
    update(newOptions: FocusTrapOptions = {}) {
      options = newOptions
    },
    destroy() {
      if (typeof window !== 'undefined') {
        window.removeEventListener('keydown', handleKeydown, true)
      }
      if (trigger && typeof trigger.focus === 'function') {
        setTimeout(() => {
          const doc = typeof document !== 'undefined' ? document : null
          if (!doc) return
          const isConnected = typeof doc.contains === 'function' ? doc.contains(trigger) : true
          const isUsable = !(trigger as any).disabled && (trigger as any).getAttribute?.('aria-hidden') !== 'true'
          if (isConnected && isUsable) {
            trigger.focus()
          }
        }, 0)
      }
    }
  }
}
