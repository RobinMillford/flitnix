import test from 'node:test'
import assert from 'node:assert/strict'
import { translate, registerCatalog, tFn, setLocale, resolveLocale, locale } from '../src/i18n.ts'
import enCatalog from '../src/locales/en.json' with { type: 'json' }

test('i18n: resolves basic nested keys in canonical English catalog', () => {
  assert.equal(translate('nav.personal'), 'Personal Transfer')
  assert.equal(translate('nav.settings'), 'Settings')
  assert.equal(translate('connection.readyToPair'), 'Ready to pair')
  assert.equal(translate('settings.appearance'), 'Appearance & Theme')
  assert.equal(translate('devices.pairingModalTitle'), 'Incoming Pairing Request')
  assert.equal(translate('devices.allowAndPair'), 'Allow & Pair')
  assert.equal(translate('devices.deny'), 'Deny')
  assert.equal(translate('devices.rearmAction'), 'Re-Arm Pairing')
  assert.equal(translate('devices.pairingExpiresIn', { seconds: 120 }), 'Request expires in 120s')
})

test('i18n: interpolates parameters safely without HTML execution', () => {
  const result = translate('announcements.uploadApproved', { name: '<script>alert(1)</script>' })
  assert.equal(result, 'Upload approved for <script>alert(1)</script>.')
  assert.equal(typeof result, 'string')

  const res2 = translate('settings.currentPath', { path: '/home/user/Downloads' })
  assert.equal(res2, 'Current effective path: /home/user/Downloads')
})

test('i18n: supports pluralization rules via _one and _other', () => {
  const single = translate('announcements.itemsAdded', { count: 1 })
  assert.equal(single, '1 item added for sharing.')

  const plural = translate('announcements.itemsAdded', { count: 5 })
  assert.equal(plural, '5 items added for sharing.')
})

test('i18n: fallback chain uses English catalog for missing keys in partial locale', () => {
  registerCatalog('es', {
    nav: {
      personal: 'Transferencia Personal'
      // settings is deliberately omitted
    }
  })

  // Found in 'es'
  assert.equal(translate('nav.personal', undefined, 'es'), 'Transferencia Personal')
  // Missing in 'es' -> falls back to 'en'
  assert.equal(translate('nav.settings', undefined, 'es'), 'Settings')
})

test('i18n: missing key in all catalogs returns the raw key string', () => {
  const raw = 'nonexistent.section.key'
  assert.equal(translate(raw), raw)
})

test('i18n: non-reactive helper tFn works directly and reflects active locale', () => {
  setLocale('en')
  assert.equal(tFn('app.title'), 'Flitnix')
  assert.equal(tFn('doctor.title'), 'Connection Doctor')
})

test('i18n: resolveLocale resolves system locale with safe English fallback and supports explicit override', () => {
  // 1. Explicit override 'en' always pins to English regardless of system language
  assert.equal(resolveLocale('en', 'es-ES'), 'en')
  assert.equal(resolveLocale('en', 'de-DE'), 'en')

  // 2. 'system' with English browser language resolves to 'en'
  assert.equal(resolveLocale('system', 'en-US'), 'en')
  assert.equal(resolveLocale('system', 'en'), 'en')

  // 3. 'system' with unsupported browser language safely falls back to 'en'
  assert.equal(resolveLocale('system', 'fr-FR'), 'en')
  assert.equal(resolveLocale('system', 'ja-JP'), 'en')

  // 4. 'system' with supported catalog resolves to that catalog
  registerCatalog('es', {
    nav: { personal: 'Personal' }
  })
  assert.equal(resolveLocale('system', 'es-ES'), 'es')
  assert.equal(resolveLocale('system', 'es'), 'es')
})

test('i18n: setLocale updates active locale store', () => {
  let active = ''
  const unsub = locale.subscribe((val) => { active = val })
  
  setLocale('en')
  assert.equal(active, 'en')

  setLocale('es')
  assert.equal(active, 'es')

  setLocale('system')
  // In node environment without navigator, falls back to 'en'
  assert.equal(active, 'en')

  unsub()
})

test('i18n Catalog Integrity: en.json contains valid non-empty strings and balanced braces', () => {
  assert.ok(enCatalog && typeof enCatalog === 'object')

  let keyCount = 0

  function validateNode(node: any, path: string) {
    for (const [k, v] of Object.entries(node)) {
      const currentPath = path ? `${path}.${k}` : k
      if (typeof v === 'string') {
        keyCount++
        assert.ok(v.trim().length > 0, `Translation key "${currentPath}" is empty!`)
        
        // Check for balanced braces {param}
        const openBraces = (v.match(/\{/g) || []).length
        const closeBraces = (v.match(/\}/g) || []).length
        assert.equal(
          openBraces,
          closeBraces,
          `Translation key "${currentPath}" has unbalanced braces: "${v}"`
        )
      } else if (typeof v === 'object' && v !== null) {
        validateNode(v, currentPath)
      } else {
        assert.fail(`Invalid value type for key "${currentPath}": ${typeof v}`)
      }
    }
  }

  validateNode(enCatalog, '')
  assert.ok(keyCount >= 80, `Expected at least 80 translation keys, found ${keyCount}`)
})
