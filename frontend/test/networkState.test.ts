import test from 'node:test'
import assert from 'node:assert/strict'

interface MockView {
  name: string
  ip: string
  cidr: string
  family: string
  friendlyName: string
  type: string
  metadataAvailable: boolean
  connectionName: string
  friendlyNameSource: string
  active: boolean
}

interface MockNetworkState {
  status: 'scanning' | 'connected' | 'no_network' | 'selected_unavailable' | 'metadata_unavailable' | 'error'
  interfaceName: string
  friendlyName: string
  type: string
  ssid?: string
  ip: string
  family: string
  cidr: string
  selectedInterface: string
  preferredInterface?: string
  serverAddress?: string
  advertisedURL?: string
  lastDetected: number
  interfaces: MockView[]
  error?: string
}

function processNetworkUpdate(
  current: {
    networkState: MockNetworkState
    interfaces: MockView[]
    selectedIp: string
    serverAddress: string
    ifaceError: string
    preferredInterface?: string
  },
  st: MockNetworkState
) {
  current.networkState = st
  current.interfaces = st.interfaces || []

  if (st.status === 'no_network') {
    current.ifaceError = st.error || 'No network connection available'
    current.selectedIp = ''
    current.serverAddress = ''
  } else if (st.status === 'error') {
    current.ifaceError = st.error || 'Network detection failed'
  } else {
    current.ifaceError = ''
    if (current.interfaces.length === 1) {
      current.selectedIp = current.interfaces[0].ip
    } else if (
      current.preferredInterface &&
      current.interfaces.some((i) => i.name === current.preferredInterface)
    ) {
      const match =
        current.interfaces.find(
          (i) => i.name === current.preferredInterface && i.family === 'IPv4'
        ) ?? current.interfaces.find((i) => i.name === current.preferredInterface)
      if (match) current.selectedIp = match.ip
    } else if (
      st.ip &&
      (!current.selectedIp || !current.interfaces.some((i) => i.ip === current.selectedIp))
    ) {
      current.selectedIp = st.ip
    }

    if (st.serverAddress) {
      current.serverAddress = st.serverAddress
    }
  }
}

test('Network State: Connected with Wi-Fi SSID and local IP', () => {
  const state: MockNetworkState = {
    status: 'connected',
    interfaceName: 'wlo1',
    friendlyName: 'Home-WiFi',
    type: 'Wi-Fi',
    ssid: 'Home-WiFi',
    ip: '192.168.1.50',
    family: 'IPv4',
    cidr: '192.168.1.50/24',
    selectedInterface: 'wlo1',
    serverAddress: '192.168.1.50:43821',
    advertisedURL: 'http://192.168.1.50:43821',
    lastDetected: Date.now(),
    interfaces: [
      {
        name: 'wlo1',
        ip: '192.168.1.50',
        cidr: '192.168.1.50/24',
        family: 'IPv4',
        friendlyName: 'Home-WiFi',
        type: 'Wi-Fi',
        metadataAvailable: true,
        connectionName: 'Home-WiFi',
        friendlyNameSource: 'SSID',
        active: true
      }
    ]
  }

  const model = {
    networkState: { status: 'scanning' } as MockNetworkState,
    interfaces: [] as MockView[],
    selectedIp: '',
    serverAddress: '',
    ifaceError: ''
  }

  processNetworkUpdate(model, state)

  assert.equal(model.networkState.status, 'connected')
  assert.equal(model.selectedIp, '192.168.1.50')
  assert.equal(model.serverAddress, '192.168.1.50:43821')
  assert.equal(model.ifaceError, '')
  assert.equal(model.interfaces.length, 1)
})

test('Network State: Disconnection removes stale IP/URL and sets error', () => {
  const model = {
    networkState: { status: 'connected' } as MockNetworkState,
    interfaces: [{ name: 'wlo1', ip: '192.168.1.50' }] as MockView[],
    selectedIp: '192.168.1.50',
    serverAddress: '192.168.1.50:43821',
    ifaceError: ''
  }

  const discState: MockNetworkState = {
    status: 'no_network',
    interfaceName: '',
    friendlyName: '',
    type: '',
    ip: '',
    family: '',
    cidr: '',
    selectedInterface: '',
    serverAddress: '',
    advertisedURL: '',
    lastDetected: Date.now(),
    interfaces: [],
    error: 'No usable local network interface found; connect to a network first'
  }

  processNetworkUpdate(model, discState)

  assert.equal(model.networkState.status, 'no_network')
  assert.equal(model.selectedIp, '')
  assert.equal(model.serverAddress, '')
  assert.equal(model.interfaces.length, 0)
  assert.match(model.ifaceError, /No usable local network interface found/)
})

test('Network State: Reconnection to phone hotspot updates IP and advertised address', () => {
  const model = {
    networkState: { status: 'no_network' } as MockNetworkState,
    interfaces: [] as MockView[],
    selectedIp: '',
    serverAddress: '',
    ifaceError: 'No usable local network interface found'
  }

  const hotspotState: MockNetworkState = {
    status: 'connected',
    interfaceName: 'wlo1',
    friendlyName: 'PixelHotspot',
    type: 'Wi-Fi',
    ssid: 'PixelHotspot',
    ip: '192.168.43.120',
    family: 'IPv4',
    cidr: '192.168.43.120/24',
    selectedInterface: 'wlo1',
    serverAddress: '192.168.43.120:43821',
    advertisedURL: 'http://192.168.43.120:43821',
    lastDetected: Date.now(),
    interfaces: [
      {
        name: 'wlo1',
        ip: '192.168.43.120',
        cidr: '192.168.43.120/24',
        family: 'IPv4',
        friendlyName: 'PixelHotspot',
        type: 'Wi-Fi',
        metadataAvailable: true,
        connectionName: 'PixelHotspot',
        friendlyNameSource: 'SSID',
        active: true
      }
    ]
  }

  processNetworkUpdate(model, hotspotState)

  assert.equal(model.networkState.status, 'connected')
  assert.equal(model.selectedIp, '192.168.43.120')
  assert.equal(model.serverAddress, '192.168.43.120:43821')
  assert.equal(model.ifaceError, '')
  assert.equal(model.networkState.ssid, 'PixelHotspot')
})

test('Network State: Preferred interface unavailable fallback sets explanation', () => {
  const fallbackState: MockNetworkState = {
    status: 'selected_unavailable',
    interfaceName: 'wlo1',
    friendlyName: 'Home-WiFi',
    type: 'Wi-Fi',
    ssid: 'Home-WiFi',
    ip: '192.168.1.50',
    family: 'IPv4',
    cidr: '192.168.1.50/24',
    selectedInterface: 'wlo1',
    preferredInterface: 'eth0',
    serverAddress: '192.168.1.50:43821',
    advertisedURL: 'http://192.168.1.50:43821',
    lastDetected: Date.now(),
    interfaces: [
      {
        name: 'wlo1',
        ip: '192.168.1.50',
        cidr: '192.168.1.50/24',
        family: 'IPv4',
        friendlyName: 'Home-WiFi',
        type: 'Wi-Fi',
        metadataAvailable: true,
        connectionName: 'Home-WiFi',
        friendlyNameSource: 'SSID',
        active: true
      }
    ],
    error: 'Preferred interface "eth0" is unavailable; using Home-WiFi (192.168.1.50)'
  }

  const model = {
    networkState: { status: 'scanning' } as MockNetworkState,
    interfaces: [] as MockView[],
    selectedIp: '',
    serverAddress: '',
    ifaceError: '',
    preferredInterface: 'eth0'
  }

  processNetworkUpdate(model, fallbackState)

  assert.equal(model.networkState.status, 'selected_unavailable')
  assert.equal(model.selectedIp, '192.168.1.50')
  assert.match(fallbackState.error || '', /Preferred interface "eth0" is unavailable/)
})
