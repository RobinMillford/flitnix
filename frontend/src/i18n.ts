/**
 * Lightweight, zero-dependency localization (i18n) engine for Flitnix.
 *
 * Provides:
 * - Reactive Svelte translation store: $t(key, params)
 * - Reactive locale store: $locale
 * - Fallback chain: requested locale -> English default -> raw key
 * - Pluralization: _one vs _other suffix when params.count is provided
 * - Safe parameter interpolation: replaces {param} without HTML execution
 */

import { writable, derived } from 'svelte/store'
import enCatalog from './locales/en.json' with { type: 'json' }

export type Translations = Record<string, any>

const catalogs: Record<string, Translations> = {
  en: enCatalog
}

export const locale = writable<string>('en')

/**
 * Resolves a dot-notation key (e.g. "nav.personal") within a dictionary.
 */
function resolveKey(obj: any, path: string): string | null {
  if (!obj || typeof obj !== 'object') return null
  const segments = path.split('.')
  let current = obj
  for (const seg of segments) {
    if (current && typeof current === 'object' && seg in current) {
      current = current[seg]
    } else {
      return null
    }
  }
  return typeof current === 'string' ? current : null
}

/**
 * Interpolates {key} parameters safely as strings.
 */
function interpolate(template: string, params?: Record<string, any>): string {
  if (!params) return template
  return template.replace(/\{(\w+)\}/g, (match, paramName) => {
    if (paramName in params) {
      const val = params[paramName]
      return val !== null && val !== undefined ? String(val) : ''
    }
    return match
  })
}

/**
 * Translates a key with optional parameters, taking pluralization and fallback into account.
 */
export function translate(key: string, params?: Record<string, any>, currentLocale = 'en'): string {
  const activeCatalog = catalogs[currentLocale] || catalogs['en']
  const fallbackCatalog = catalogs['en']

  let candidate: string | null = null

  // 1. Pluralization check if count is present
  if (params && typeof params.count === 'number') {
    const pluralKey = `${key}_${params.count === 1 ? 'one' : 'other'}`
    candidate = resolveKey(activeCatalog, pluralKey)
    if (!candidate && activeCatalog !== fallbackCatalog) {
      candidate = resolveKey(fallbackCatalog, pluralKey)
    }
  }

  // 2. Direct key lookup
  if (!candidate) {
    candidate = resolveKey(activeCatalog, key)
  }

  // 3. Fallback to English catalog if different
  if (!candidate && activeCatalog !== fallbackCatalog) {
    candidate = resolveKey(fallbackCatalog, key)
  }

  // 4. Fallback to key itself
  const text = candidate ?? key

  return interpolate(text, params)
}

let currentActiveLocale = 'en'
locale.subscribe((loc) => { currentActiveLocale = loc })

/**
 * Resolves a locale identifier, translating 'system' using the browser's language preferences.
 * Checks full tag (e.g. 'en-us'), base tag (e.g. 'en'), and falls back to 'en'.
 */
export function resolveLocale(targetLocale: string, navLang?: string): string {
  if (!targetLocale || targetLocale === 'system') {
    const langStr = navLang !== undefined ? navLang : (typeof navigator !== 'undefined' && navigator.language ? navigator.language : '')
    if (langStr) {
      const full = langStr.toLowerCase()
      if (full in catalogs) return full
      const base = full.split('-')[0]
      if (base in catalogs) return base
    }
    return 'en'
  }
  return targetLocale
}

/**
 * Sets the active locale. Handles 'system' by resolving against available catalogs.
 */
export function setLocale(newLocale: string, navLang?: string): void {
  const resolved = resolveLocale(newLocale, navLang)
  locale.set(resolved)
}

/**
 * Registers an additional translation catalog dynamically.
 */
export function registerCatalog(lang: string, catalog: Translations): void {
  catalogs[lang] = catalog
}

/**
 * Non-reactive translate function using the currently active locale.
 */
export function tFn(key: string, params?: Record<string, any>): string {
  return translate(key, params, currentActiveLocale)
}

/**
 * Derived reactive translation store for Svelte components: $t(key, params)
 */
export const t = derived(locale, ($locale) => {
  return (key: string, params?: Record<string, any>) => translate(key, params, $locale)
})
