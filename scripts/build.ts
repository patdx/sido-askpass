import { chmodSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { transformSync } from 'amaro'

mkdirSync(new URL('../dist', import.meta.url), { recursive: true })

for (const name of ['shared', 'sido', 'sido-askpass']) {
  const source = readFileSync(
    new URL(`../src/${name}.ts`, import.meta.url),
    'utf8',
  )
  const { code } = transformSync(source, { mode: 'strip-only' })
  const output = new URL(`../dist/${name}.js`, import.meta.url)
  writeFileSync(
    output,
    code.replace("from './shared.ts'", "from './shared.js'"),
  )
  if (name !== 'shared') chmodSync(output, 0o755)
}
