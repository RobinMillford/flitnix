import test from 'node:test'
import assert from 'node:assert/strict'

interface TextMessage {
  id: string
  sender: 'desktop' | 'phone'
  content: string
  isLink: boolean
  url?: string
  createdAt: string
}

function parseTextMessageResponse(data: any): TextMessage[] {
  if (!data) return []
  if (Array.isArray(data.messages)) return data.messages
  if (Array.isArray(data)) return data
  return []
}

function processIncomingTextMessages(
  incoming: TextMessage[],
  knownIds: Set<string>,
  initialized: boolean
): {
  updatedKnownIds: Set<string>
  newPhoneNotes: TextMessage[]
  newPhoneLinks: TextMessage[]
} {
  const newPhoneNotes: TextMessage[] = []
  const newPhoneLinks: TextMessage[] = []
  const updatedKnownIds = new Set(knownIds)

  if (!initialized) {
    incoming.forEach((m) => {
      if (m?.id) updatedKnownIds.add(m.id)
    })
    return { updatedKnownIds, newPhoneNotes, newPhoneLinks }
  }

  for (const m of incoming) {
    if (m?.id && !updatedKnownIds.has(m.id)) {
      updatedKnownIds.add(m.id)
      if (m.sender === 'phone') {
        if (m.isLink) {
          newPhoneLinks.push(m)
        } else {
          newPhoneNotes.push(m)
        }
      }
    }
  }

  return { updatedKnownIds, newPhoneNotes, newPhoneLinks }
}

test('Quick Text: Parses { messages: [...] } response payload from API', () => {
  const payload = {
    messages: [
      {
        id: 'msg-1',
        sender: 'phone',
        content: 'Check https://flitnix.org',
        isLink: true,
        url: 'https://flitnix.org',
        createdAt: '2026-11-08T10:00:00Z'
      },
      {
        id: 'msg-2',
        sender: 'desktop',
        content: 'Meeting notes here',
        isLink: false,
        createdAt: '2026-11-08T10:01:00Z'
      }
    ]
  }

  const result = parseTextMessageResponse(payload)
  assert.equal(result.length, 2)
  assert.equal(result[0].id, 'msg-1')
  assert.equal(result[1].id, 'msg-2')
})

test('Quick Text: Parses direct array response payload gracefully', () => {
  const payload = [
    {
      id: 'msg-1',
      sender: 'desktop',
      content: 'Direct array note',
      isLink: false,
      createdAt: '2026-11-08T10:00:00Z'
    }
  ]

  const result = parseTextMessageResponse(payload)
  assert.equal(result.length, 1)
  assert.equal(result[0].content, 'Direct array note')
})

test('Quick Text: Empty or malformed payload returns empty array without throwing', () => {
  assert.deepEqual(parseTextMessageResponse(null), [])
  assert.deepEqual(parseTextMessageResponse(undefined), [])
  assert.deepEqual(parseTextMessageResponse({}), [])
  assert.deepEqual(parseTextMessageResponse('invalid string'), [])
})

test('Quick Text: Initial load registers existing message IDs without firing notifications', () => {
  const existing: TextMessage[] = [
    {
      id: 'msg-1',
      sender: 'phone',
      content: 'Existing note',
      isLink: false,
      createdAt: '2026-11-08T10:00:00Z'
    }
  ]

  const known = new Set<string>()
  const res = processIncomingTextMessages(existing, known, false)

  assert.equal(res.updatedKnownIds.has('msg-1'), true)
  assert.equal(res.newPhoneNotes.length, 0)
  assert.equal(res.newPhoneLinks.length, 0)
})

test('Quick Text: Subsequent poll detects new incoming notes and links from phone', () => {
  const known = new Set<string>(['msg-1'])

  const pollResult: TextMessage[] = [
    {
      id: 'msg-1',
      sender: 'phone',
      content: 'Existing note',
      isLink: false,
      createdAt: '2026-11-08T10:00:00Z'
    },
    {
      id: 'msg-2',
      sender: 'desktop',
      content: 'Desktop sent note',
      isLink: false,
      createdAt: '2026-11-08T10:02:00Z'
    },
    {
      id: 'msg-3',
      sender: 'phone',
      content: 'New note from mobile',
      isLink: false,
      createdAt: '2026-11-08T10:03:00Z'
    },
    {
      id: 'msg-4',
      sender: 'phone',
      content: 'https://flitnix.org',
      isLink: true,
      url: 'https://flitnix.org',
      createdAt: '2026-11-08T10:04:00Z'
    }
  ]

  const res = processIncomingTextMessages(pollResult, known, true)

  assert.equal(res.updatedKnownIds.size, 4)
  assert.equal(res.newPhoneNotes.length, 1)
  assert.equal(res.newPhoneNotes[0].id, 'msg-3')
  assert.equal(res.newPhoneLinks.length, 1)
  assert.equal(res.newPhoneLinks[0].id, 'msg-4')
  // Desktop sender should not trigger phone notification
  assert.equal(res.newPhoneNotes.some((n) => n.id === 'msg-2'), false)
})

test('Quick Text: Ordering reverse presentation places newest message first', () => {
  const messages: TextMessage[] = [
    { id: '1', sender: 'desktop', content: 'First note', isLink: false, createdAt: '2026-11-08T10:00:00Z' },
    { id: '2', sender: 'phone', content: 'Second note', isLink: false, createdAt: '2026-11-08T10:05:00Z' },
    { id: '3', sender: 'phone', content: 'Third note', isLink: false, createdAt: '2026-11-08T10:10:00Z' }
  ]

  const reversed = [...messages].reverse()
  assert.equal(reversed[0].id, '3')
  assert.equal(reversed[1].id, '2')
  assert.equal(reversed[2].id, '1')
})
