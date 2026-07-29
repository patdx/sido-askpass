import { readFileSync, writeFileSync, mkdirSync, chmodSync } from 'node:fs'
import { transformSync } from 'amaro'

const src = readFileSync(new URL('../sido-askpass.ts', import.meta.url), 'utf8')
const { code } = transformSync(src, { mode: 'strip-only' })
mkdirSync(new URL('../dist', import.meta.url), { recursive: true })
const out = new URL('../dist/sido-askpass.js', import.meta.url)
writeFileSync(out, code)
chmodSync(out, 0o755)
