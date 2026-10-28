# Flitnix Desktop Frontend

The desktop user interface for Flitnix, built with [Svelte 5](https://svelte.dev/), [TypeScript](https://www.typescriptlang.org/), and [Vite](https://vitejs.dev/).

## Structure

- `src/App.svelte`: Main desktop UI component (pairing, active session, file transfer queue).
- `src/refreshScheduler.ts`: Coalesced UI refresh scheduler handling IPC state updates from the Go backend.
- `src/assets/`: Embedded Nunito web font and branding assets.
- `wailsjs/`: Auto-generated TypeScript bindings to Go backend methods.

## Development

```bash
# Run standalone Vite development server
npm run dev

# Run type check and unit tests
npm run check
npm test

# Build production bundle for Wails
npm run build
```
